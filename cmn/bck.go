// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn/cos"
)

type (
	// Ns (or Namespace) adds additional layer for scoping the data under
	// the same provider. It allows to have same dataset and bucket names
	// under different namespaces what allows for easy data manipulation without
	// affecting data in different namespaces.
	Ns struct {
		// UUID of other remote AIS cluster (for now only used for AIS). Note
		// that we can have different namespaces which refer to same UUID (cluster).
		// This means that in a sense UUID is a parent of the actual namespace.
		UUID string `json:"uuid" yaml:"uuid"`
		// Name uniquely identifies a namespace under the same UUID (which may
		// be empty) and is used in building FQN for the objects.
		Name string `json:"name" yaml:"name"`
	}

	Bck struct {
		Name     string       `json:"name" yaml:"name"`
		Provider string       `json:"provider" yaml:"provider"`
		Ns       Ns           `json:"namespace" yaml:"namespace" list:"omitempty"`
		Props    *BucketProps `json:"-"`
	}

	// Represents the AIS bucket, object and URL associated with a HTTP resource
	HTTPBckObj struct {
		Bck        Bck
		ObjName    string
		OrigURLBck string // HTTP URL of the bucket (object name excluded)
	}

	QueryBcks Bck

	Bcks []Bck

	// implemented by cluster.Bck
	NLP interface {
		Lock()
		TryLock(timeout time.Duration) bool
		TryRLock(timeout time.Duration) bool
		Unlock()
	}

	ParseURIOpts struct {
		DefaultProvider string // If set the provider will be used as provider.
		IsQuery         bool   // Determines if the URI should be parsed as query.
	}
)

const (
	// NsGlobalUname is hardcoded here to avoid allocating it via Uname()
	// (the most common use case)
	NsGlobalUname = "@#"
)

var (
	// NsGlobal represents *this* cluster's global namespace that is used by default when
	// no specific namespace was defined or provided by the user.
	NsGlobal = Ns{}
	// NsAnyRemote represents any remote cluster. As such, NsGlobalRemote applies
	// exclusively to AIS (provider) given that other Backend providers are remote by definition.
	NsAnyRemote = Ns{UUID: string(apc.NsUUIDPrefix)}
)

////////////////
// Validation //
////////////////

// Validation for buckets is split into 2 cases:
//  1. Validation of concrete bucket, eg. get(bck, objName). In this case we
//     require the bucket name to be set. If the provider is not set the default
//     will be used, see `NormalizeProvider`.
//     This case is handled in `newBckFromQuery` and `newBckFromQueryUname`. The
//     CLI counterpart is `parseBckURI`.
//  2. Validation of query buckets, eg. list(queryBcks). Here all parts of the
//     bucket all optional.
//     This case is handled in `newQueryBcksFromQuery`. The CLI counterpart is
//     `parseQueryBckURI`.
// These 2 cases have a slightly different logic for the validation but the
// validation functions are always the same. Bucket name (`bck.ValidateName`)
// and bucket namespace (`bck.Ns.Validate`) validation is quite straightforward
// as we only need to check if the strings contain only valid characters. Bucket
// provider validation on the other hand a little bit more tricky as we have so
// called "normalized providers" and their aliases. Normalized providers are the
// providers registered in `Providers` set. Almost any provider that is being
// validated goes through `NormalizeProvider` which converts aliases to
// normalized form or sets default provider if the provider is empty. But there
// are cases where we already expect **only** the normalized providers, for
// example in FQN parsing. For this case `IsNormalizedProvider` function must be
// used.
//
// Similar concepts are applied when bucket is provided as URI,
// eg. `ais://@uuid#ns/bucket_name`. URI form is heavily used by CLI. Parsing
// is handled by `ParseBckObjectURI` which by itself doesn't do much validation.
// The validation happens in aforementioned CLI specific parse functions.

// IsNormalizedProvider returns true if the provider is in normalized
// form (`aws`, `gcp`, etc.), not aliased (`s3`, `gs`, etc.). Only providers
// registered in `Providers` set are considered normalized.
func IsNormalizedProvider(provider string) bool {
	_, exists := apc.Providers[provider]
	return exists
}

// NormalizeProvider replaces provider aliases with their normalized form/name.
func NormalizeProvider(provider string) (string, error) {
	if IsNormalizedProvider(provider) {
		return provider, nil
	}
	switch provider {
	case "":
		return apc.ProviderAIS, nil // NOTE: ais is the default provider
	case apc.S3Scheme:
		return apc.ProviderAmazon, nil
	case apc.AZScheme:
		return apc.ProviderAzure, nil
	case apc.GSScheme:
		return apc.ProviderGoogle, nil
	default:
		return provider, NewErrorInvalidBucketProvider(Bck{Provider: provider})
	}
}

////////
// Ns //
////////

// Parses [@uuid][#namespace]. It does a little bit more than just parsing
// a string from `Uname` so that logic can be reused in different places.
func ParseNsUname(s string) (n Ns) {
	if s == NsGlobalUname {
		return NsGlobal // to speedup the common case (here and elsewhere)
	}
	if len(s) > 0 && s[0] == apc.NsUUIDPrefix {
		s = s[1:]
	}
	idx := strings.IndexByte(s, apc.NsNamePrefix)
	if idx == -1 {
		n.UUID = s
	} else {
		n.UUID = s[:idx]
		n.Name = s[idx+1:]
	}
	return
}

func (n Ns) String() (res string) {
	if n.IsGlobal() {
		return
	}
	if n.UUID != "" {
		res += string(apc.NsUUIDPrefix) + n.UUID
	}
	if n.Name != "" {
		res += string(apc.NsNamePrefix) + n.Name
	}
	return
}

func (n Ns) Uname() string {
	if n.IsGlobal() {
		return NsGlobalUname
	}
	b := make([]byte, 0, 2+len(n.UUID)+len(n.Name))
	b = append(b, apc.NsUUIDPrefix)
	b = append(b, n.UUID...)
	b = append(b, apc.NsNamePrefix)
	b = append(b, n.Name...)
	return string(b)
}

func (n Ns) Validate() error {
	if n.IsGlobal() {
		return nil
	}
	if cos.IsAlphaPlus(n.UUID, false /*with period*/) && cos.IsAlphaPlus(n.Name, false) {
		return nil
	}
	return fmt.Errorf(
		"namespace (uuid: %q, name: %q) may only contain letters, numbers, dashes (-), underscores (_)",
		n.UUID, n.Name,
	)
}

func (n Ns) Contains(other Ns) bool {
	if n.IsGlobal() {
		return true // If query is empty (global) we accept any namespace
	}
	if n.IsAnyRemote() {
		return other.IsRemote()
	}
	return n == other
}

/////////
// Bck (value)
/////////

func (b Bck) Equal(other *Bck) bool {
	return b.Name == other.Name && b.Provider == other.Provider && b.Ns == other.Ns
}

func (b Bck) String() string {
	if b.Ns.IsGlobal() {
		if b.Provider == "" {
			return b.Name
		}
		return b.Provider + apc.BckProviderSeparator + b.Name
	}
	if b.Provider == "" {
		return fmt.Sprintf("%s/%s", b.Ns, b.Name)
	}
	return fmt.Sprintf("%s%s%s/%s", b.Provider, apc.BckProviderSeparator, b.Ns, b.Name)
}

func (b Bck) StringEx() string {
	if b.Provider == "" {
		// ais:// is the default
		if b.Ns.IsGlobal() {
			return apc.ProviderAIS + apc.BckProviderSeparator + b.Name
		}
		return fmt.Sprintf("%s%s%s/%s", apc.ProviderAIS, apc.BckProviderSeparator, b.Ns, b.Name)
	}
	return b.String()
}

// unique name => Bck (use MakeUname above to perform the reverse translation)
func ParseUname(uname string) (b Bck, objName string) {
	var prev, itemIdx int
	for i := 0; i < len(uname); i++ {
		if uname[i] != filepath.Separator {
			continue
		}

		item := uname[prev:i]
		switch itemIdx {
		case 0:
			b.Provider = item
		case 1:
			b.Ns = ParseNsUname(item)
		case 2:
			b.Name = item
			objName = uname[i+1:]
			return
		}

		itemIdx++
		prev = i + 1
	}
	return
}

/////////
// Bck (ref)
/////////

func (b *Bck) Copy(src *Bck) { *b = *src }

func (b *Bck) Less(other *Bck) bool {
	if QueryBcks(*b).Contains(other) {
		return true
	}
	if b.Provider != other.Provider {
		return b.Provider < other.Provider
	}
	sb, so := b.Ns.String(), other.Ns.String()
	if sb != so {
		return sb < so
	}
	return b.Name < other.Name
}

func (b *Bck) Validate() (err error) {
	err = b.ValidateName()
	if err == nil {
		err = b.Ns.Validate()
	}
	return
}

func (b *Bck) ValidateName() (err error) {
	if b.Name == "" || b.Name == "." {
		return fmt.Errorf(fmtErrBckName, b.Name)
	}
	if !cos.IsAlphaPlus(b.Name, true /*with period*/) {
		err = fmt.Errorf(fmtErrBckName, b.Name)
	}
	return
}

func (b *Bck) IsEmpty() bool {
	return b == nil || (b.Name == "" && b.Provider == "" && b.Ns == NsGlobal)
}

// QueryBcks (see below) is a Bck that _can_ have an empty Name.
func (b *Bck) IsQuery() bool { return b.Name == "" }

// Bck => unique name (use ParseUname below to translate back)
func (b *Bck) MakeUname(objName string) string {
	var (
		nsUname = b.Ns.Uname()
		l       = len(b.Provider) + 1 + len(nsUname) + 1 + len(b.Name) + 1 + len(objName)
		buf     = make([]byte, 0, l)
	)
	buf = append(buf, b.Provider...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, nsUname...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, b.Name...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, objName...)
	return cos.UnsafeS(buf)
}

//
// Is-Whats
//

func IsCloudProvider(p string) bool {
	return p == apc.ProviderAmazon || p == apc.ProviderGoogle || p == apc.ProviderAzure
}

func IsRemoteProvider(p string) bool {
	return IsCloudProvider(p) || p == apc.ProviderHDFS || p == apc.ProviderHTTP
}

func (n Ns) IsGlobal() bool    { return n == NsGlobal }
func (n Ns) IsAnyRemote() bool { return n == NsAnyRemote }
func (n Ns) IsRemote() bool    { return n.UUID != "" }

func (b *Bck) Backend() *Bck {
	bprops := b.Props
	if bprops == nil {
		return nil
	}
	if bprops.BackendBck.Name == "" {
		return nil
	}
	return &bprops.BackendBck
}

func (b *Bck) RemoteBck() *Bck {
	if bck := b.Backend(); bck != nil {
		return bck
	}
	if IsRemoteProvider(b.Provider) || b.IsRemoteAIS() {
		return b
	}
	return nil
}

func (b *Bck) IsAIS() bool {
	return b.Provider == apc.ProviderAIS && !b.Ns.IsRemote() && b.Backend() == nil
}

func (b *Bck) IsRemoteAIS() bool { return b.Provider == apc.ProviderAIS && b.Ns.IsRemote() }
func (b *Bck) IsHDFS() bool      { return b.Provider == apc.ProviderHDFS }
func (b *Bck) IsHTTP() bool      { return b.Provider == apc.ProviderHTTP }

func (b *Bck) IsRemote() bool {
	return IsRemoteProvider(b.Provider) || b.IsRemoteAIS() || b.Backend() != nil
}

func (b *Bck) IsCloud() bool {
	if IsCloudProvider(b.Provider) {
		return true
	}
	backend := b.Backend()
	if backend == nil {
		return false
	}
	return IsCloudProvider(backend.Provider)
}

func (b *Bck) HasProvider() bool { return b.Provider != "" }

//
// useful helpers
//

func (b *Bck) AddToQuery(query url.Values) url.Values {
	if b.Provider != "" {
		if query == nil {
			query = make(url.Values)
		}
		query.Set(apc.QparamProvider, b.Provider)
	}
	if !b.Ns.IsGlobal() {
		if query == nil {
			query = make(url.Values)
		}
		query.Set(apc.QparamNamespace, b.Ns.Uname())
	}
	return query
}

func (b *Bck) AddUnameToQuery(query url.Values, uparam string) url.Values {
	if query == nil {
		query = make(url.Values)
	}
	uname := b.MakeUname("")
	query.Set(uparam, uname)
	return query
}

func DelBckFromQuery(query url.Values) url.Values {
	query.Del(apc.QparamProvider)
	query.Del(apc.QparamNamespace)
	return query
}

///////////////
// QueryBcks //
///////////////

// QueryBcks is a Bck that _can_ have an empty Name. (TODO: extend to support prefix and regex.)
func (qbck *QueryBcks) IsBucket() bool { return !(*Bck)(qbck).IsQuery() }

func (qbck QueryBcks) String() string { b := Bck(qbck); return b.String() }

func (qbck *QueryBcks) IsAIS() bool       { b := (*Bck)(qbck); return b.IsAIS() }
func (qbck *QueryBcks) IsHDFS() bool      { b := (*Bck)(qbck); return b.IsHDFS() }
func (qbck *QueryBcks) IsRemoteAIS() bool { b := (*Bck)(qbck); return b.IsRemoteAIS() }
func (qbck *QueryBcks) IsCloud() bool     { return IsCloudProvider(qbck.Provider) }

func (qbck *QueryBcks) AddToQuery(query url.Values) url.Values {
	bck := (*Bck)(qbck)
	return bck.AddToQuery(query)
}

func (qbck *QueryBcks) Validate() (err error) {
	if qbck.Name != "" {
		bck := Bck(*qbck)
		if err := bck.ValidateName(); err != nil {
			return err
		}
	}
	if qbck.Provider != "" {
		qbck.Provider, err = NormalizeProvider(qbck.Provider)
		if err != nil {
			return err
		}
	}
	if qbck.Ns != NsGlobal && qbck.Ns != NsAnyRemote {
		return qbck.Ns.Validate()
	}
	return nil
}

func (qbck QueryBcks) Equal(bck *Bck) bool { return Bck(qbck).Equal(bck) }

func (qbck QueryBcks) Contains(other *Bck) bool {
	if qbck.Name != "" {
		// NOTE: named bucket with no provider is assumed to be ais://
		if other.Provider == "" {
			other.Provider = apc.ProviderAIS
		}
		if qbck.Provider == "" {
			// If qbck's provider not set, we should match the expected bucket
			qbck.Provider = other.Provider // nolint:revive // temp change to compare
		}
		return qbck.Equal(other)
	}
	ok := qbck.Provider == other.Provider || qbck.Provider == ""
	return ok && qbck.Ns.Contains(other.Ns)
}

//////////
// Bcks //
//////////

// interface guard
var _ sort.Interface = (*Bcks)(nil)

func (bcks Bcks) Len() int {
	return len(bcks)
}

func (bcks Bcks) Less(i, j int) bool {
	return bcks[i].Less(&bcks[j])
}

func (bcks Bcks) Swap(i, j int) {
	bcks[i], bcks[j] = bcks[j], bcks[i]
}

func (bcks Bcks) Select(query QueryBcks) (filtered Bcks) {
	for _, bck := range bcks {
		if query.Contains(&bck) {
			filtered = append(filtered, bck)
		}
	}
	return filtered
}

func (bcks Bcks) Contains(query QueryBcks) bool {
	for _, bck := range bcks {
		if query.Equal(&bck) || query.Contains(&bck) {
			return true
		}
	}
	return false
}

func (bcks Bcks) Equal(other Bcks) bool {
	if len(bcks) != len(other) {
		return false
	}
	for _, b1 := range bcks {
		var found bool
		for _, b2 := range other {
			if b1.Equal(&b2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

////////////////
// HTTPBckObj //
////////////////

func NewHTTPObj(u *url.URL) *HTTPBckObj {
	hbo := &HTTPBckObj{
		Bck: Bck{
			Provider: apc.ProviderHTTP,
			Ns:       NsGlobal,
		},
	}
	hbo.OrigURLBck, hbo.ObjName = filepath.Split(u.Path)
	hbo.OrigURLBck = u.Scheme + apc.BckProviderSeparator + u.Host + hbo.OrigURLBck
	hbo.Bck.Name = OrigURLBck2Name(hbo.OrigURLBck)
	return hbo
}

func NewHTTPObjPath(rawURL string) (*HTTPBckObj, error) {
	urlObj, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, err
	}
	return NewHTTPObj(urlObj), nil
}
