// Package transport provides streaming object-based transport over http for intra-cluster continuous
// intra-cluster communications (see README for details and usage example).
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package transport

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/OneOfOne/xxhash"
	"github.com/pierrec/lz4/v3"
)

const hkOld = time.Hour

// private types
type (
	iterator struct {
		handler *handler
		body    io.Reader
		hbuf    []byte
		pdu     *rpdu
		stats   *Stats
	}
	objReader struct {
		body   io.Reader
		off    int64
		hdr    ObjHdr
		pdu    *rpdu
		loghdr string
	}
	handler struct {
		trname      string
		rxObj       ReceiveObj
		rxMsg       ReceiveMsg
		sessions    sync.Map // map[uint64]*Stats
		oldSessions sync.Map // map[uint64]time.Time
		hkName      string   // house-keeping name
		mm          *memsys.MMSA
	}

	ErrDuplicateTrname struct {
		trname string
	}
)

var (
	nextSID  atomic.Int64        // next unique session ID
	handlers map[string]*handler // by trname
	mu       *sync.RWMutex       // ptotect handlers
)

// main Rx objects
func RxAnyStream(w http.ResponseWriter, r *http.Request) {
	var (
		reader    io.Reader = r.Body
		lz4Reader *lz4.Reader
		trname    = path.Base(r.URL.Path)
	)
	mu.RLock()
	h, ok := handlers[trname]
	if !ok {
		mu.RUnlock()
		err := cmn.NewErrNotFound("unknown transport endpoint %q", trname)
		if verbose {
			cmn.WriteErr(w, r, err, 0)
		} else {
			cmn.WriteErr(w, r, err, 0, 1 /*silent*/)
		}
		return
	}
	mu.RUnlock()
	// compression
	if compressionType := r.Header.Get(apc.HdrCompress); compressionType != "" {
		debug.Assert(compressionType == apc.LZ4Compression)
		lz4Reader = lz4.NewReader(r.Body)
		reader = lz4Reader
	}

	// session
	sessID, err := strconv.ParseInt(r.Header.Get(apc.HdrSessID), 10, 64)
	if err != nil || sessID == 0 {
		cmn.WriteErr(w, r, fmt.Errorf("%s[:%d]: invalid session ID, err %v", trname, sessID, err))
		return
	}
	uid := uniqueID(r, sessID)
	statsif, _ := h.sessions.LoadOrStore(uid, &Stats{})
	xxh, _ := UID2SessID(uid)
	loghdr := fmt.Sprintf("%s[%d:%d]", trname, xxh, sessID)
	if verbose {
		glog.Infof("%s: start-of-stream from %s", loghdr, r.RemoteAddr)
	}
	stats := statsif.(*Stats)

	// receive loop
	hbuf, _ := h.mm.AllocSize(int64(maxHeaderSize))
	it := &iterator{handler: h, body: reader, hbuf: hbuf, stats: stats}
	err = it.rxloop(uid, loghdr)

	// cleanup
	if lz4Reader != nil {
		lz4Reader.Reset(nil)
	}
	if it.pdu != nil {
		it.pdu.free(h.mm)
	}
	h.mm.Free(hbuf)

	// if err != io.EOF {
	if !cos.IsEOF(err) {
		cmn.WriteErr(w, r, err)
	}
}

////////////////////////
// ErrDuplicateTrname //
////////////////////////

func (e *ErrDuplicateTrname) Error() string { return fmt.Sprintf("duplicate trname %q", e.trname) }
func IsErrDuplicateTrname(e error) bool     { _, ok := e.(*ErrDuplicateTrname); return ok }

/////////////
// handler //
/////////////

func (h *handler) handle() error {
	mu.Lock()
	if _, ok := handlers[h.trname]; ok {
		mu.Unlock()
		return &ErrDuplicateTrname{h.trname}
	}
	handlers[h.trname] = h
	mu.Unlock()
	hk.Reg(h.hkName+hk.NameSuffix, h.cleanupOldSessions, hkOld)
	return nil
}

func (h *handler) cleanupOldSessions() time.Duration {
	now := mono.NanoTime()
	f := func(key, value interface{}) bool {
		uid := key.(uint64)
		timeClosed := value.(int64)
		if time.Duration(now-timeClosed) > hkOld {
			h.oldSessions.Delete(uid)
			h.sessions.Delete(uid)
		}
		return true
	}
	h.oldSessions.Range(f)
	return hkOld
}

//////////////
// iterator //
//////////////

func (it *iterator) Read(p []byte) (n int, err error) { return it.body.Read(p) }

func (it *iterator) rxloop(uid uint64, loghdr string) (err error) {
	h := it.handler
	for err == nil {
		var (
			obj   *objReader
			msg   Msg
			flags uint64
			hlen  int
		)
		hlen, flags, err = it.nextProtoHdr(loghdr)
		if err != nil {
			break
		}
		if lh := len(it.hbuf); hlen > lh {
			err = fmt.Errorf("sbr1 %s: hlen %d exceeds buflen=%d", loghdr, hlen, lh)
			break
		}
		_ = it.stats.Offset.Add(int64(hlen + sizeProtoHdr))
		if flags&msgFlag == 0 {
			obj, err = it.nextObj(loghdr, hlen)
			if obj != nil {
				if flags&firstPDU != 0 && !obj.hdr.IsHeaderOnly() {
					if it.pdu == nil {
						buf, _ := h.mm.AllocSize(MaxSizePDU)
						it.pdu = newRecvPDU(it.body, buf)
					}
					obj.pdu = it.pdu
					obj.pdu.reset()
				}
				err = eofOK(err)
				size, off := obj.hdr.ObjAttrs.Size, obj.off
				if errCb := h.rxObj(obj.hdr, obj, err); errCb != nil {
					err = errCb
				}
				// stats
				if err == nil {
					it.stats.Num.Inc()              // this stream stats
					statsTracker.Add(InObjCount, 1) // stats/target_stats.go
					if size >= 0 {
						statsTracker.Add(InObjSize, size)
					} else {
						debug.Assert(size == SizeUnknown)
						statsTracker.Add(InObjSize, obj.off-off)
					}
				}
			} else if err != nil && err != io.EOF {
				if errCb := h.rxObj(ObjHdr{}, nil, err); errCb != nil {
					err = errCb
				}
			}
		} else {
			msg, err = it.nextMsg(loghdr, hlen)
			if err == nil {
				err = h.rxMsg(msg, nil)
			} else if err != io.EOF {
				err = h.rxMsg(Msg{}, err)
			}
		}
	}
	h.oldSessions.Store(uid, mono.NanoTime())
	return
}

func eofOK(err error) error {
	if err == io.EOF {
		err = nil
	}
	return err
}

// nextProtoHdr receives and handles 16 bytes of the protocol header (not to confuse with transport.Obj.Hdr)
// returns hlen, which is header length - for transport.Obj, and message length - for transport.Msg
func (it *iterator) nextProtoHdr(loghdr string) (hlen int, flags uint64, err error) {
	var n int
	n, err = it.Read(it.hbuf[:sizeProtoHdr])
	if n < sizeProtoHdr {
		if err == nil {
			err = fmt.Errorf("sbr3 %s: failed to receive proto hdr (n=%d)", loghdr, n)
		}
		return
	}
	// extract and validate hlen
	hlen, flags, err = extProtoHdr(it.hbuf, loghdr)
	return
}

func (it *iterator) nextObj(loghdr string, hlen int) (obj *objReader, err error) {
	var n int
	n, err = it.Read(it.hbuf[:hlen])
	if n < hlen {
		if err == nil {
			err = fmt.Errorf("sbr4 %s: failed to receive obj hdr (%d < %d)", loghdr, n, hlen)
		}
		return
	}
	hdr := ExtObjHeader(it.hbuf, hlen)
	if hdr.isFin() {
		err = io.EOF
		return
	}
	obj = allocRecv()
	obj.body, obj.hdr, obj.loghdr = it.body, hdr, loghdr
	return
}

func (it *iterator) nextMsg(loghdr string, hlen int) (msg Msg, err error) {
	var n int
	n, err = it.Read(it.hbuf[:hlen])
	if n < hlen {
		if err == nil {
			err = fmt.Errorf("sbr5 %s: failed to receive msg (%d < %d)", loghdr, n, hlen)
		}
		return
	}
	debug.Assertf(n == hlen, "%d != %d", n, hlen)
	msg = ExtMsg(it.hbuf, hlen)
	if msg.isFin() {
		err = io.EOF
	}
	return
}

///////////////
// objReader //
///////////////

func (obj *objReader) Read(b []byte) (n int, err error) {
	if obj.pdu != nil {
		return obj.readPDU(b)
	}
	debug.Assert(obj.Size() >= 0)
	rem := obj.Size() - obj.off
	if rem < int64(len(b)) {
		b = b[:int(rem)]
	}
	n, err = obj.body.Read(b)
	obj.off += int64(n) // NOTE: `GORACE` complaining here can be safely ignored
	switch err {
	case nil:
		if obj.off >= obj.Size() {
			err = io.EOF
		}
	case io.EOF:
		if obj.off != obj.Size() {
			err = fmt.Errorf("sbr6 %s: premature eof %d != %s, err %w", obj.loghdr, obj.off, obj, err)
		}
	default:
		err = fmt.Errorf("sbr7 %s: off %d, obj %s, err %w", obj.loghdr, obj.off, obj, err)
	}
	return
}

func (obj *objReader) String() string {
	return fmt.Sprintf("%s(size=%d)", obj.hdr.FullName(), obj.Size())
}

func (obj *objReader) Size() int64     { return obj.hdr.ObjSize() }
func (obj *objReader) IsUnsized() bool { return obj.hdr.IsUnsized() }

//
// pduReader
//

func (obj *objReader) readPDU(b []byte) (n int, err error) {
	pdu := obj.pdu
	if pdu.woff == 0 {
		err = pdu.readHdr(obj.loghdr)
		if err != nil {
			return
		}
	}
	for !pdu.done {
		if _, err = pdu.readFrom(); err != nil && err != io.EOF {
			err = fmt.Errorf("sbr8 %s: failed to receive PDU, err %w, obj %s", obj.loghdr, err, obj)
			break
		}
		debug.Assert(err == nil || (err == io.EOF && pdu.done))
		if !pdu.done {
			runtime.Gosched()
		}
	}
	n = pdu.read(b)
	obj.off += int64(n)

	if err != nil {
		return
	}
	if pdu.rlength() == 0 {
		if pdu.last {
			err = io.EOF
			if obj.IsUnsized() {
				obj.hdr.ObjAttrs.Size = obj.off
			} else if obj.Size() != obj.off {
				glog.Errorf("sbr9 %s: off %d != %s", obj.loghdr, obj.off, obj)
			}
		} else {
			pdu.reset()
		}
	}
	return
}

//
// session ID <=> unique ID
//

func uniqueID(r *http.Request, sessID int64) uint64 {
	x := xxhash.ChecksumString64S(r.RemoteAddr, cos.MLCG32)
	return (x&math.MaxUint32)<<32 | uint64(sessID)
}

func UID2SessID(uid uint64) (xxh, sessID uint64) {
	xxh, sessID = uid>>32, uid&math.MaxUint32
	return
}

// DrainAndFreeReader:
// 1) reads and discards all the data from `r` - the `objReader`;
// 2) frees this objReader back to the `recvPool`.
// As such, this function is intended for usage only and exclusively by
// `transport.ReceiveObj` implementations.
func DrainAndFreeReader(r io.Reader) {
	if r == nil {
		return
	}
	obj, ok := r.(*objReader)
	debug.Assert(ok)
	if obj.body != nil && !obj.hdr.IsHeaderOnly() {
		cos.DrainReader(obj)
	}
	FreeRecv(obj)
}
