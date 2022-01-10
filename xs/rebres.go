// Package xs contains eXtended actions (xactions) except storage services
// (mirror, ec) and extensions (downloader, lru).
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xact"
	"github.com/NVIDIA/aistore/xact/xreg"
)

// rebalance & resilver xactions

type (
	rebFactory struct {
		xreg.RenewBase
		xctn *Rebalance
	}
	resFactory struct {
		xreg.RenewBase
		xctn *Resilver
	}

	Rebalance struct {
		xact.Base
	}
	Resilver struct {
		xact.Base
	}
)

// interface guard
var (
	_ cluster.Xact   = (*Rebalance)(nil)
	_ xreg.Renewable = (*rebFactory)(nil)

	_ cluster.Xact   = (*Resilver)(nil)
	_ xreg.Renewable = (*resFactory)(nil)
)

///////////////
// Rebalance //
///////////////

func (*rebFactory) New(args xreg.Args, _ *cluster.Bck) xreg.Renewable {
	return &rebFactory{RenewBase: xreg.RenewBase{Args: args}}
}

func (p *rebFactory) Start() error {
	p.xctn = NewRebalance(p.Args.UUID, p.Kind())
	return nil
}

func (*rebFactory) Kind() string        { return cmn.ActRebalance }
func (p *rebFactory) Get() cluster.Xact { return p.xctn }

func (p *rebFactory) WhenPrevIsRunning(prevEntry xreg.Renewable) (wpr xreg.WPR, err error) {
	xreb := prevEntry.(*rebFactory)
	wpr = xreg.WprAbort
	if xreb.Args.UUID > p.Args.UUID {
		glog.Errorf("(reb: %s) %s is greater than %s", xreb.xctn, xreb.Args.UUID, p.Args.UUID)
		wpr = xreg.WprUse
	} else if xreb.Args.UUID == p.Args.UUID {
		if verbose {
			glog.Infof("%s already running, nothing to do", xreb.xctn)
		}
		wpr = xreg.WprUse
	}
	return
}

func NewRebalance(id, kind string) (xctn *Rebalance) {
	xctn = &Rebalance{}
	xctn.InitBase(id, kind, nil)
	return
}

func (*Rebalance) Run(*sync.WaitGroup) { debug.Assert(false) }

func (xctn *Rebalance) Snap() cluster.XactSnap {
	rebSnap := &stats.RebalanceSnap{}
	xctn.ToSnap(&rebSnap.Snap)
	if marked := xreg.GetRebMarked(); marked.Xact != nil {
		id, err := xact.S2RebID(marked.Xact.ID())
		debug.AssertNoErr(err)
		rebSnap.RebID = id
	} else {
		rebSnap.RebID = 0
	}
	// NOTE: the number of rebalanced objects _is_ the number of transmitted objects
	//       (definition)
	rebSnap.Stats.Objs = rebSnap.Stats.OutObjs
	rebSnap.Stats.Bytes = rebSnap.Stats.OutBytes
	return rebSnap
}

//////////////
// Resilver //
//////////////

func (*resFactory) New(args xreg.Args, _ *cluster.Bck) xreg.Renewable {
	return &resFactory{RenewBase: xreg.RenewBase{Args: args}}
}

func (p *resFactory) Start() error {
	p.xctn = NewResilver(p.UUID(), p.Kind())
	return nil
}

func (*resFactory) Kind() string                                       { return cmn.ActResilver }
func (p *resFactory) Get() cluster.Xact                                { return p.xctn }
func (*resFactory) WhenPrevIsRunning(xreg.Renewable) (xreg.WPR, error) { return xreg.WprAbort, nil }

func NewResilver(id, kind string) (xctn *Resilver) {
	xctn = &Resilver{}
	xctn.InitBase(id, kind, nil)
	return
}

func (*Resilver) Run(*sync.WaitGroup) { debug.Assert(false) }

// TODO -- FIXME: check "resilver-marked" and unify with rebalance
func (xctn *Resilver) Snap() cluster.XactSnap {
	baseStats := xctn.Base.Snap().(*xact.Snap)
	return baseStats
}
