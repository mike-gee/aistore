// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/feat"
	"github.com/NVIDIA/aistore/xact"
)

type bckInitArgs struct {
	p       *proxy
	w       http.ResponseWriter
	r       *http.Request
	perms   apc.AccessAttrs // apc.AceGET, apc.AcePATCH etc.
	reqBody []byte          // request body of original request

	// URL query: the conventional/slow and
	// the fast alternative tailored exclusively for the datapath
	query url.Values
	dpq   *dpq

	origURLBck string
	bck        *cluster.Bck
	msg        *apc.ActionMsg

	skipBackend bool // initialize bucket via `bck.InitNoBackend`
	createAIS   bool // create ais bucket on the fly

	headRemB    bool // handle ErrRemoteBckNotFound to discover _remote_ bucket on the fly (and add to BMD)
	tryHeadRemB bool // when listing objects anonymously (via ListObjsMsg.Flags LsTryHeadRemB)

	exists bool // true if bucket already exists
}

////////////////
// ibargsPool //
////////////////

var (
	ibargsPool sync.Pool
	ib0        bckInitArgs
)

func allocInitBckArgs() (a *bckInitArgs) {
	if v := ibargsPool.Get(); v != nil {
		a = v.(*bckInitArgs)
		return
	}
	return &bckInitArgs{}
}

func freeInitBckArgs(a *bckInitArgs) {
	*a = ib0
	ibargsPool.Put(a)
}

//
// lookup and add buckets on the fly
//

func shouldHeadRemB() bool { return !cmn.Features.IsSet(feat.NoHeadRemB) }

// args.init initializes bucket and checks access permissions.
func (args *bckInitArgs) init(bckName string) (bck *cluster.Bck, errCode int, err error) {
	if args.bck == nil {
		args.bck, err = newBckFromQ(bckName, args.query, args.dpq)
		if err != nil {
			errCode = http.StatusBadRequest
			return
		}
	}

	bck = args.bck
	if err = args._checkRemoteBckPermissions(); err != nil {
		errCode = http.StatusBadRequest
		return
	}
	if args.skipBackend {
		err = bck.InitNoBackend(args.p.owner.bmd)
	} else {
		err = bck.Init(args.p.owner.bmd)
	}
	if err != nil {
		errCode = http.StatusBadRequest
		if cmn.IsErrBucketNought(err) {
			errCode = http.StatusNotFound
		}
		return
	}

	args.bck = bck
	args.exists = true

	// if permissions are not explicitly specified check the default (msg.Action => permissions)
	if args.perms == 0 && args.msg != nil {
		xactRecord, ok := xact.Table[args.msg.Action]
		if !ok || xactRecord.Access == 0 {
			return
		}
		args.perms = xactRecord.Access
	}
	errCode, err = args.access(bck)
	return
}

// FIXME: must be provider-specific.
func (args *bckInitArgs) _checkRemoteBckPermissions() (err error) {
	if !args.bck.IsRemote() {
		return
	}
	if args._requiresPermission(apc.AceMoveBucket) {
		goto retErr
	}
	// HDFS buckets are allowed to be deleted.
	if args.bck.IsHDFS() {
		return
	}
	// HTTP buckets should fail on PUT and bucket rename operations
	if args.bck.IsHTTP() && args._requiresPermission(apc.AcePUT) {
		goto retErr
	}
	// Destroy and Rename/Move are not permitted.
	if args.bck.IsCloud() && args._requiresPermission(apc.AceDestroyBucket) &&
		args.msg.Action == apc.ActDestroyBck {
		goto retErr
	}

	return
retErr:
	op := "operation"
	if args.msg != nil {
		op = fmt.Sprintf("operation %q", args.msg.Action)
	}
	err = fmt.Errorf(cmn.FmtErrUnsupported, args.bck, op)
	return
}

func (args *bckInitArgs) _requiresPermission(perm apc.AccessAttrs) bool {
	return (args.perms & perm) == perm
}

func (args *bckInitArgs) access(bck *cluster.Bck) (errCode int, err error) {
	err = args.p.access(args.r.Header, bck, args.perms)
	errCode = aceErrToCode(err)
	return
}

// initAndTry initializes bucket and then _tries_ to add it if it doesn't exist.
// NOTE: on error the method calls `p.writeErr` - make sure _not_ to do the same in the caller
func (args *bckInitArgs) initAndTry(bucket string) (bck *cluster.Bck, err error) {
	var errCode int
	bck, errCode, err = args.init(bucket)
	if err == nil {
		return
	}
	if errCode != http.StatusNotFound {
		args.p.writeErr(args.w, args.r, err, errCode)
		return
	}
	if !cmn.IsErrBucketNought(err) {
		args.p.writeErr(args.w, args.r, err, http.StatusBadRequest)
		return
	}
	// create ais bucket on the fly?
	if cmn.IsErrBckNotFound(err) /* ais bucket not found*/ && !args.createAIS {
		args.p.writeErr(args.w, args.r, err, errCode)
		return
	}
	// create remote bucket on the fly?  (creation with respect to BMD, that is)
	if cmn.IsErrRemoteBckNotFound(err) && !args.headRemB {
		args.p.writeErrSilent(args.w, args.r, err, errCode)
		return
	}

	bck, err = args.try()
	return
}

func (args *bckInitArgs) try() (bck *cluster.Bck, err error) {
	bck, errCode, err := args._try()
	if err != nil && err != errForwarded {
		if cmn.IsErrBucketAlreadyExists(err) {
			glog.Errorf("%s: %v - race, proceeding anyway...", args.p.si, err)
			err = nil
		} else {
			args.p.writeErr(args.w, args.r, err, errCode)
		}
	}
	return bck, err
}

//
// methods that are internal to this source
//

func (args *bckInitArgs) _try() (bck *cluster.Bck, errCode int, err error) {
	if err = args.bck.Validate(); err != nil {
		errCode = http.StatusBadRequest
		return
	}

	// In case of HDFS if the bucket does not exist in BMD there is no point
	// in checking if it exists remotely if we don't have `ref_directory`.
	if args.bck.IsHDFS() {
		err = cmn.NewErrBckNotFound(args.bck.Bucket())
		errCode = http.StatusNotFound
		return
	}

	if args.p.forwardCP(args.w, args.r, args.msg, "add-bucket", args.reqBody) {
		err = errForwarded
		return
	}

	// From this point on it's the primary - lookup via random target and try bucket add to BMD.
	bck = args.bck
	action := apc.ActCreateBck

	if backend := bck.Backend(); backend != nil {
		bck = backend
	}
	if bck.IsAIS() {
		glog.Warningf("%s: %q doesn't exist, proceeding to create", args.p.si, args.bck)
	}

	var remoteProps http.Header
	if bck.IsRemote() {
		action = apc.ActAddRemoteBck
		if remoteProps, errCode, err = args._lookup(bck); err != nil {
			bck = nil
			return
		}
	}

	if bck.IsHTTP() {
		if args.origURLBck != "" {
			remoteProps.Set(apc.HdrOrigURLBck, args.origURLBck)
		} else if origURL := args.getOrigURL(); origURL != "" {
			hbo, err := cmn.NewHTTPObjPath(origURL)
			if err != nil {
				errCode = http.StatusBadRequest
				return bck, errCode, err
			}
			remoteProps.Set(apc.HdrOrigURLBck, hbo.OrigURLBck)
		} else {
			err = fmt.Errorf("failed to initialize bucket %q: missing HTTP URL", args.bck)
			errCode = http.StatusBadRequest
			return
		}
		debug.Assert(remoteProps.Get(apc.HdrOrigURLBck) != "")
	}

	if err = args.p.createBucket(&apc.ActionMsg{Action: action}, bck, remoteProps); err != nil {
		if _, ok := err.(*cmn.ErrBucketAlreadyExists); !ok {
			errCode = http.StatusConflict
			return
		}
		return
	}

	// Init the bucket after having successfully added it to the BMD.
	if err = bck.Init(args.p.owner.bmd); err != nil {
		errCode = http.StatusInternalServerError
		err = cmn.NewErrFailedTo(args.p, "add-remote", bck, err, errCode)
	}
	bck = args.bck
	return
}

func (args *bckInitArgs) getOrigURL() (ourl string) {
	if args.query != nil {
		debug.Assert(args.dpq == nil)
		ourl = args.query.Get(apc.QparamOrigURL)
	} else {
		ourl = args.dpq.origURL
	}
	return
}

// NOTE: alternatively, skip HEAD altogether when lsDontHeadRemoteBucket
func (args *bckInitArgs) _lookup(bck *cluster.Bck) (hdr http.Header, code int, err error) {
	q := url.Values{}
	if bck.IsHTTP() {
		origURL := args.getOrigURL()
		q.Set(apc.QparamOrigURL, origURL)
	}
	if args.tryHeadRemB {
		q.Set(apc.QparamSilent, "true")
	}
	hdr, code, err = args.p.headRemoteBck(bck.Bucket(), q)
	if (code == http.StatusUnauthorized || code == http.StatusForbidden) && args.tryHeadRemB {
		glog.Warningf("proceeding to add cloud bucket %s to the BMD after having failed HEAD request", bck)
		glog.Warningf("%s properties: using all defaults", bck)
		hdr = make(http.Header, 2)
		hdr.Set(apc.HdrBackendProvider, bck.Provider)
		hdr.Set(apc.HdrBucketVerEnabled, "false")
		err = nil
	}
	return
}
