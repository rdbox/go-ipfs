package bitswap

import (
	"context"
	"sync"
	"time"

	engine "github.com/ipfs/go-ipfs/exchange/bitswap/decision"
	bsmsg "github.com/ipfs/go-ipfs/exchange/bitswap/message"
	bsnet "github.com/ipfs/go-ipfs/exchange/bitswap/network"
	wantlist "github.com/ipfs/go-ipfs/exchange/bitswap/wantlist"

	metrics "gx/ipfs/QmRg1gKTHzc3CZXSKzem8aR4E3TubFhbgXwfVuWnSK5CC5/go-metrics-interface"
	cid "gx/ipfs/QmYhQaCYEcaPPjxJX7YcPcVKkQfRy6sJ7B3XmGFk82XYdQ/go-cid"
	peer "gx/ipfs/QmdS9KpbDyPrieswibZhkod1oXqRwZJrUPzxCofAMWpFGq/go-libp2p-peer"
)

type WantManager struct {
	// sync channels for Run loop
	incoming   chan *wantSet
	connect    chan peer.ID        // notification channel for new peers connecting
	disconnect chan peer.ID        // notification channel for peers disconnecting
	peerReqs   chan chan []peer.ID // channel to request connected peers on

	// synchronized by Run loop, only touch inside there
	peers map[peer.ID]*msgQueue
	wl    *wantlist.ThreadSafe

	network bsnet.BitSwapNetwork
	ctx     context.Context
	cancel  func()

	wantlistGauge metrics.Gauge
	sentHistogram metrics.Histogram
}

func NewWantManager(ctx context.Context, network bsnet.BitSwapNetwork) *WantManager {
	ctx, cancel := context.WithCancel(ctx)
	wantlistGauge := metrics.NewCtx(ctx, "wantlist_total",
		"Number of items in wantlist.").Gauge()
	sentHistogram := metrics.NewCtx(ctx, "sent_all_blocks_bytes", "Histogram of blocks sent by"+
		" this bitswap").Histogram(metricsBuckets)
	return &WantManager{
		incoming:      make(chan *wantSet, 10),
		connect:       make(chan peer.ID, 10),
		disconnect:    make(chan peer.ID, 10),
		peerReqs:      make(chan chan []peer.ID),
		peers:         make(map[peer.ID]*msgQueue),
		wl:            wantlist.NewThreadSafe(),
		network:       network,
		ctx:           ctx,
		cancel:        cancel,
		wantlistGauge: wantlistGauge,
		sentHistogram: sentHistogram,
	}
}

type msgPair struct {
	to  peer.ID
	msg bsmsg.BitSwapMessage
}

type cancellation struct {
	who peer.ID
	blk *cid.Cid
}

type msgQueue struct {
	p peer.ID

	outlk   sync.Mutex
	out     bsmsg.BitSwapMessage
	network bsnet.BitSwapNetwork
	wl      *wantlist.Wantlist

	sender bsnet.MessageSender

	refcnt int

	work chan struct{}
	done chan struct{}
}

func (pm *WantManager) WantBlocks(ctx context.Context, ks []*cid.Cid) {
	log.Infof("want blocks: %s", ks)
	pm.addEntries(ctx, ks, false)
}

func (pm *WantManager) CancelWants(ks []*cid.Cid) {
	pm.addEntries(context.Background(), ks, true)
}

type wantSet struct {
	entries []*bsmsg.Entry
	targets []peer.ID
}

func (pm *WantManager) addEntries(ctx context.Context, ks []*cid.Cid, cancel bool) {
	var entries []*bsmsg.Entry
	for i, k := range ks {
		entries = append(entries, &bsmsg.Entry{
			Cancel: cancel,
			Entry: &wantlist.Entry{
				Cid:      k,
				Priority: kMaxPriority - i,
				RefCnt:   1,
			},
		})
	}
	select {
	case pm.incoming <- &wantSet{entries: entries}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

func (pm *WantManager) ConnectedPeers() []peer.ID {
	resp := make(chan []peer.ID)
	pm.peerReqs <- resp
	return <-resp
}

func (pm *WantManager) SendBlock(ctx context.Context, env *engine.Envelope) {
	// Blocks need to be sent synchronously to maintain proper backpressure
	// throughout the network stack
	defer env.Sent()

	pm.sentHistogram.Observe(float64(len(env.Block.RawData())))

	msg := bsmsg.New(false)
	msg.AddBlock(env.Block)
	log.Infof("Sending block %s to %s", env.Block, env.Peer)
	err := pm.network.SendMessage(ctx, env.Peer, msg)
	if err != nil {
		log.Infof("sendblock error: %s", err)
	}
}

func (pm *WantManager) startPeerHandler(p peer.ID) *msgQueue {
	mq, ok := pm.peers[p]
	if ok {
		mq.refcnt++
		return nil
	}

	mq = pm.newMsgQueue(p)

	// new peer, we will want to give them our full wantlist
	fullwantlist := bsmsg.New(true)
	for _, e := range pm.wl.Entries() {
		ne := *e
		mq.wl.AddEntry(&ne)
		fullwantlist.AddEntry(e.Cid, e.Priority)
	}
	mq.out = fullwantlist
	mq.work <- struct{}{}

	pm.peers[p] = mq
	go mq.runQueue(pm.ctx)
	return mq
}

func (pm *WantManager) stopPeerHandler(p peer.ID) {
	pq, ok := pm.peers[p]
	if !ok {
		// TODO: log error?
		return
	}

	pq.refcnt--
	if pq.refcnt > 0 {
		return
	}

	close(pq.done)
	delete(pm.peers, p)
}

func (mq *msgQueue) runQueue(ctx context.Context) {
	defer func() {
		if mq.sender != nil {
			mq.sender.Close()
		}
	}()
	for {
		select {
		case <-mq.work: // there is work to be done
			mq.doWork(ctx)
		case <-mq.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (mq *msgQueue) doWork(ctx context.Context) {
	if mq.sender == nil {
		err := mq.openSender(ctx)
		if err != nil {
			log.Infof("cant open message sender to peer %s: %s", mq.p, err)
			// TODO: cant connect, what now?
			return
		}
	}

	// grab outgoing message
	mq.outlk.Lock()
	wlm := mq.out
	if wlm == nil || wlm.Empty() {
		mq.outlk.Unlock()
		return
	}
	mq.out = nil
	mq.outlk.Unlock()

	// send wantlist updates
	for { // try to send this message until we fail.
		err := mq.sender.SendMsg(ctx, wlm)
		if err == nil {
			return
		}

		log.Infof("bitswap send error: %s", err)
		mq.sender.Close()
		mq.sender = nil

		select {
		case <-mq.done:
			return
		case <-ctx.Done():
			return
		case <-time.After(time.Millisecond * 100):
			// wait 100ms in case disconnect notifications are still propogating
			log.Warning("SendMsg errored but neither 'done' nor context.Done() were set")
		}

		err = mq.openSender(ctx)
		if err != nil {
			log.Errorf("couldnt open sender again after SendMsg(%s) failed: %s", mq.p, err)
			// TODO(why): what do we do now?
			// I think the *right* answer is to probably put the message we're
			// trying to send back, and then return to waiting for new work or
			// a disconnect.
			return
		}

		// TODO: Is this the same instance for the remote peer?
		// If its not, we should resend our entire wantlist to them
		/*
			if mq.sender.InstanceID() != mq.lastSeenInstanceID {
				wlm = mq.getFullWantlistMessage()
			}
		*/
	}
}

func (mq *msgQueue) openSender(ctx context.Context) error {
	// allow ten minutes for connections this includes looking them up in the
	// dht dialing them, and handshaking
	conctx, cancel := context.WithTimeout(ctx, time.Minute*10)
	defer cancel()

	err := mq.network.ConnectTo(conctx, mq.p)
	if err != nil {
		return err
	}

	nsender, err := mq.network.NewMessageSender(ctx, mq.p)
	if err != nil {
		return err
	}

	mq.sender = nsender
	return nil
}

func (pm *WantManager) Connected(p peer.ID) {
	select {
	case pm.connect <- p:
	case <-pm.ctx.Done():
	}
}

func (pm *WantManager) Disconnected(p peer.ID) {
	select {
	case pm.disconnect <- p:
	case <-pm.ctx.Done():
	}
}

// TODO: use goprocess here once i trust it
func (pm *WantManager) Run() {
	tock := time.NewTicker(rebroadcastDelay.Get())
	defer tock.Stop()
	for {
		select {
		case ws := <-pm.incoming:

			// add changes to our wantlist
			for _, e := range ws.entries {
				if e.Cancel {
					if pm.wl.Remove(e.Cid) {
						pm.wantlistGauge.Dec()
					}
				} else {
					if pm.wl.AddEntry(e.Entry) {
						pm.wantlistGauge.Inc()
					}
				}
			}

			// broadcast those wantlist changes
			if len(ws.targets) == 0 {
				for _, p := range pm.peers {
					p.addMessage(ws.entries)
				}
			} else {
				for _, t := range ws.targets {
					p, ok := pm.peers[t]
					if !ok {
						log.Warning("tried sending wantlist change to non-partner peer")
						continue
					}
					p.addMessage(ws.entries)
				}
			}

		case <-tock.C:
			// resend entire wantlist every so often (REALLY SHOULDNT BE NECESSARY)
			var es []*bsmsg.Entry
			for _, e := range pm.wl.Entries() {
				es = append(es, &bsmsg.Entry{Entry: e})
			}

			for _, p := range pm.peers {
				p.outlk.Lock()
				p.out = bsmsg.New(true)
				p.outlk.Unlock()

				p.addMessage(es)
			}
		case p := <-pm.connect:
			pm.startPeerHandler(p)
		case p := <-pm.disconnect:
			pm.stopPeerHandler(p)
		case req := <-pm.peerReqs:
			var peers []peer.ID
			for p := range pm.peers {
				peers = append(peers, p)
			}
			req <- peers
		case <-pm.ctx.Done():
			return
		}
	}
}

func (wm *WantManager) newMsgQueue(p peer.ID) *msgQueue {
	return &msgQueue{
		done:    make(chan struct{}),
		work:    make(chan struct{}, 1),
		wl:      wantlist.New(),
		network: wm.network,
		p:       p,
		refcnt:  1,
	}
}

func (mq *msgQueue) addMessage(entries []*bsmsg.Entry) {
	var work bool
	mq.outlk.Lock()
	defer func() {
		mq.outlk.Unlock()
		if !work {
			return
		}
		select {
		case mq.work <- struct{}{}:
		default:
		}
	}()

	// if we have no message held allocate a new one
	if mq.out == nil {
		mq.out = bsmsg.New(false)
	}

	// TODO: add a msg.Combine(...) method
	// otherwise, combine the one we are holding with the
	// one passed in
	for _, e := range entries {
		if e.Cancel {
			if mq.wl.Remove(e.Cid) {
				work = true
				mq.out.Cancel(e.Cid)
			}
		} else {
			if mq.wl.Add(e.Cid, e.Priority) {
				work = true
				mq.out.AddEntry(e.Cid, e.Priority)
			}
		}
	}
}
