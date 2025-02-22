// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/ais/s3compat"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/memsys"
	jsoniter "github.com/json-iterator/go"
)

var (
	errS3Req = errors.New("invalid s3 request")
	errS3Obj = errors.New("missing or empty object name")
)

// [METHOD] /s3
func (p *proxy) s3Handler(w http.ResponseWriter, r *http.Request) {
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("S3Request: %s - %s", r.Method, r.URL)
	}

	// TODO: Fix the hack, https://github.com/tensorflow/tensorflow/issues/41798
	cos.ReparseQuery(r)
	apiItems, err := p.checkRESTItems(w, r, 0, true, apc.URLPathS3.L)
	if err != nil {
		return
	}

	switch r.Method {
	case http.MethodHead:
		if len(apiItems) == 0 {
			p.writeErr(w, r, errS3Req)
			return
		}
		if len(apiItems) == 1 {
			p.headBckS3(w, r, apiItems[0])
			return
		}
		p.headObjS3(w, r, apiItems)
	case http.MethodGet:
		if len(apiItems) == 0 {
			// nothing  - list all the buckets
			p.bckNamesToS3(w)
			return
		}
		q := r.URL.Query()
		_, lifecycle := q[s3compat.QparamLifecycle]
		_, policy := q[s3compat.QparamPolicy]
		_, cors := q[s3compat.QparamCORS]
		_, acl := q[s3compat.QparamACL]
		if lifecycle || policy || cors || acl {
			p.unsupported(w, r, apiItems[0])
			return
		}
		if len(apiItems) == 1 {
			_, versioning := q[s3compat.QparamVersioning]
			if versioning {
				p.getBckVersioningS3(w, r, apiItems[0])
				return
			}
			// only bucket name - list objects in the bucket
			p.bckListS3(w, r, apiItems[0])
			return
		}
		// object data otherwise
		p.getObjS3(w, r, apiItems)
	case http.MethodPut:
		if len(apiItems) == 0 {
			p.writeErr(w, r, errS3Req)
			return
		}
		if len(apiItems) == 1 {
			q := r.URL.Query()
			_, versioning := q[s3compat.QparamVersioning]
			if versioning {
				p.putBckVersioningS3(w, r, apiItems[0])
				return
			}
			p.putBckS3(w, r, apiItems[0])
			return
		}
		p.putObjS3(w, r, apiItems)
	case http.MethodPost:
		if len(apiItems) != 1 {
			p.writeErr(w, r, errS3Req)
			return
		}
		q := r.URL.Query()
		if _, multiple := q[s3compat.QparamMultiDelete]; !multiple {
			p.writeErr(w, r, errS3Req)
			return
		}
		p.delMultipleObjs(w, r, apiItems[0])
	case http.MethodDelete:
		if len(apiItems) == 0 {
			p.writeErr(w, r, errS3Req)
			return
		}
		if len(apiItems) == 1 {
			q := r.URL.Query()
			_, multiple := q[s3compat.QparamMultiDelete]
			if multiple {
				p.delMultipleObjs(w, r, apiItems[0])
				return
			}
			p.delBckS3(w, r, apiItems[0])
			return
		}
		p.delObjS3(w, r, apiItems)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead,
			http.MethodPost, http.MethodPut)
	}
}

// GET s3/
func (p *proxy) bckNamesToS3(w http.ResponseWriter) {
	var (
		bmd  = p.owner.bmd.get()
		qbck = cmn.QueryBcks{Provider: apc.ProviderAIS}
		cp   = &qbck.Provider
	)
	resp := s3compat.NewListBucketResult()
	bmd.Range(cp, nil, func(bck *cluster.Bck) bool {
		if qbck.Equal(bck.Bucket()) || qbck.Contains(bck.Bucket()) {
			resp.Add(bck)
		}
		return false
	})
	sgl := memsys.PageMM().NewSGL(0)
	resp.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// PUT s3/bck-name
func (p *proxy) putBckS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Validate(); err != nil {
		p.writeErr(w, r, err)
		return
	}
	msg := apc.ActionMsg{Action: apc.ActCreateBck}
	if p.forwardCP(w, r, nil, msg.Action+"-"+bucket) {
		return
	}
	if err := p.createBucket(&msg, bck); err != nil {
		errCode := http.StatusInternalServerError
		if _, ok := err.(*cmn.ErrBucketAlreadyExists); ok {
			errCode = http.StatusConflict
		}
		p.writeErr(w, r, err, errCode)
	}
}

// DEL s3/bck-name
// TODO: AWS allows to delete bucket only if it is empty
func (p *proxy) delBckS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	msg := apc.ActionMsg{Action: apc.ActDestroyBck}
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err, http.StatusNotFound)
		return
	}
	if err := bck.Allow(apc.AceDestroyBucket); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	if p.forwardCP(w, r, nil, msg.Action+"-"+bucket) {
		return
	}
	if err := p.destroyBucket(&msg, bck); err != nil {
		errCode := http.StatusInternalServerError
		if _, ok := err.(*cmn.ErrBucketAlreadyExists); ok {
			glog.Infof("%s: %s already %q-ed, nothing to do", p, bck, msg.Action)
			return
		}
		p.writeErr(w, r, err, errCode)
	}
}

// DEL s3/bck-name?delete
// Delete list of objects
func (p *proxy) delMultipleObjs(w http.ResponseWriter, r *http.Request, bucket string) {
	defer cos.Close(r.Body)
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err, http.StatusNotFound)
		return
	}
	if err := bck.Allow(apc.AceObjDELETE); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	decoder := xml.NewDecoder(r.Body)
	objList := &s3compat.Delete{}
	if err := decoder.Decode(objList); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if len(objList.Object) == 0 {
		return
	}
	msg := apc.ActionMsg{Action: apc.ActDeleteObjects}
	query := make(url.Values)
	query.Set(apc.QparamProvider, apc.ProviderAIS)
	lrMsg := &cmn.SelectObjsMsg{ObjNames: make([]string, 0, len(objList.Object))}
	for _, obj := range objList.Object {
		lrMsg.ObjNames = append(lrMsg.ObjNames, obj.Key)
	}
	msg.Value = lrMsg
	// Marshal+Unmashal to new struct:
	// hack to make `doListRange` treat `listMsg` as `map[string]interface`
	var (
		msg2 apc.ActionMsg
		bt   = cos.MustMarshal(&msg)
	)
	if err := jsoniter.Unmarshal(bt, &msg2); err != nil {
		err = fmt.Errorf(cmn.FmtErrUnmarshal, p, "list-range action message", cos.BHead(bt), err)
		p.writeErr(w, r, err)
		return
	}
	if _, err := p.doListRange(r.Method, bucket, &msg2, query); err != nil {
		p.writeErr(w, r, err)
	}
}

// HEAD s3/bck-name
func (p *proxy) headBckS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err, http.StatusNotFound)
		return
	}
	if err := bck.Allow(apc.AceBckHEAD); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	// From AWS docs:
	// This operation is useful to determine if a bucket exists and you have
	// permission to access it. The operation returns a 200 OK if the bucket
	// exists and you have permission to access it. Otherwise, the operation
	// might return responses such as 404 Not Found and 403 Forbidden.
	//
	// But, after checking Amazon response, it appeared that Amazon adds
	// region name to the response, and at least AWS CLI uses it.
	w.Header().Set("Server", s3compat.AISSever)
	w.Header().Set("x-amz-bucket-region", s3compat.AISRegion)
}

// GET s3/bckName
func (p *proxy) bckListS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	lsmsg := apc.ListObjsMsg{UUID: cos.GenUUID(), TimeFormat: time.RFC3339}
	lsmsg.AddProps(apc.GetPropsSize, apc.GetPropsChecksum, apc.GetPropsAtime, apc.GetPropsVersion)
	s3compat.FillMsgFromS3Query(r.URL.Query(), &lsmsg)

	locationIsAIS := bck.IsAIS() || lsmsg.IsFlagSet(apc.LsPresent)
	var (
		objList *cmn.BucketList
		err     error
	)
	if locationIsAIS {
		objList, err = p.listObjectsAIS(bck, &lsmsg)
	} else {
		objList, err = p.listObjectsRemote(bck, &lsmsg)
	}
	if err != nil {
		p.writeErr(w, r, err)
		return
	}

	resp := s3compat.NewListObjectResult()
	resp.ContinuationToken = lsmsg.ContinuationToken
	resp.FillFromAisBckList(objList, &lsmsg)
	sgl := memsys.PageMM().NewSGL(0)
	resp.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// PUT s3/bckName/objName - with HeaderObjSrc in request header - a source
func (p *proxy) copyObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	started := time.Now()
	src := r.Header.Get(s3compat.HeaderObjSrc)
	src = strings.Trim(src, "/") // in examples the path starts with "/"
	parts := strings.SplitN(src, "/", 2)
	if len(parts) < 2 {
		p.writeErr(w, r, errS3Obj)
		return
	}
	bckSrc := cluster.NewBck(parts[0], apc.ProviderAIS, cmn.NsGlobal)
	if err := bckSrc.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if err := bckSrc.Allow(apc.AceGET); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	bckDst := cluster.NewBck(items[0], apc.ProviderAIS, cmn.NsGlobal)
	if err := bckDst.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if len(items) < 2 {
		p.writeErr(w, r, errS3Obj)
		return
	}
	var (
		si   *cluster.Snode
		smap = p.owner.smap.get()
		err  error
	)
	if err = bckDst.Allow(apc.AcePUT); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	objName := strings.Trim(parts[1], "/")
	si, err = cluster.HrwTarget(bckSrc.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3 COPY: %s %s/%s => %s/%v %s", r.Method, bckSrc, objName, bckDst, items, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData)
	p.s3Redirect(w, r, si, redirectURL, bckDst.Name)
}

// PUT s3/bckName/objName - without extra info in request header
func (p *proxy) directPutObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	started := time.Now()
	bck := cluster.NewBck(items[0], apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if len(items) < 2 {
		p.writeErr(w, r, errS3Obj)
		return
	}
	var (
		si   *cluster.Snode
		smap = p.owner.smap.get()
		err  error
	)
	if err = bck.Allow(apc.AcePUT); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	objName := path.Join(items[1:]...)
	si, err = cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3: %s %s/%s => %s", r.Method, bck, objName, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

// PUT s3/bckName/objName
func (p *proxy) putObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if r.Header.Get(s3compat.HeaderObjSrc) == "" {
		p.directPutObjS3(w, r, items)
		return
	}
	p.copyObjS3(w, r, items)
}

// GET s3/<bucket-name/<object-name>[?uuid=<etl-uuid>]
func (p *proxy) getObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	started := time.Now()
	bck := cluster.NewBck(items[0], apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if len(items) < 2 {
		p.writeErr(w, r, errS3Obj)
		return
	}
	var (
		si   *cluster.Snode
		smap = p.owner.smap.get()
		err  error
	)
	if err = bck.Allow(apc.AceGET); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	objName := path.Join(items[1:]...)

	si, err = cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3: %s %s/%s => %s", r.Method, bck, objName, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

func (p *proxy) headObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if len(items) < 2 {
		p.writeErr(w, r, errS3Obj)
		return
	}
	bucket, objName := items[0], path.Join(items[1:]...)
	bck := cluster.NewBck(items[0], apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if err := bck.Allow(apc.AceObjHEAD); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	smap := p.owner.smap.get()
	si, err := cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err, http.StatusInternalServerError)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3 %s %s/%s => %s", r.Method, bucket, objName, si)
	}

	p.reverseNodeRequest(w, r, si)
}

// DEL s3/bckName/objName
func (p *proxy) delObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	started := time.Now()
	bck := cluster.NewBck(items[0], apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if len(items) < 2 {
		p.writeErr(w, r, errS3Obj)
		return
	}
	var (
		si   *cluster.Snode
		smap = p.owner.smap.get()
		err  error
	)
	if err = bck.Allow(apc.AceObjDELETE); err != nil {
		p.writeErr(w, r, err, http.StatusForbidden)
		return
	}
	objName := path.Join(items[1:]...)
	si, err = cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3: %s %s/%s => %s", r.Method, bck, objName, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetIntraData)
	p.s3Redirect(w, r, si, redirectURL, bck.Name)
}

// GET s3/bk-name?versioning
func (p *proxy) getBckVersioningS3(w http.ResponseWriter, r *http.Request, bucket string) {
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	resp := s3compat.NewVersioningConfiguration(bck.Props.Versioning.Enabled)
	sgl := memsys.PageMM().NewSGL(0)
	resp.MustMarshal(sgl)
	w.Header().Set(cos.HdrContentType, cos.ContentXML)
	sgl.WriteTo(w)
	sgl.Free()
}

// GET s3/bk-name?lifecycle|cors|policy|acl
func (p *proxy) unsupported(w http.ResponseWriter, r *http.Request, bucket string) {
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

// PUT s3/bk-name?versioning
func (p *proxy) putBckVersioningS3(w http.ResponseWriter, r *http.Request, bucket string) {
	msg := &apc.ActionMsg{Action: apc.ActSetBprops}
	if p.forwardCP(w, r, nil, msg.Action+"-"+bucket) {
		return
	}
	bck := cluster.NewBck(bucket, apc.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	_, exists := p.owner.bmd.get().Get(bck)
	if !exists {
		p.writeErrMsg(w, r, "bucket does not exist", http.StatusNotFound)
		return
	}
	decoder := xml.NewDecoder(r.Body)
	defer cos.Close(r.Body)
	vconf := &s3compat.VersioningConfiguration{}
	if err := decoder.Decode(vconf); err != nil {
		p.writeErr(w, r, err)
		return
	}
	enabled := vconf.Enabled()
	propsToUpdate := cmn.BucketPropsToUpdate{
		Versioning: &cmn.VersionConfToUpdate{Enabled: &enabled},
	}
	// make and validate new props
	nprops, err := p.makeNewBckProps(bck, &propsToUpdate)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if _, err := p.setBucketProps(msg, bck, nprops); err != nil {
		p.writeErr(w, r, err)
	}
}
