package redfish

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// Redfish collections are paged: vendors cap Members per page (75 entries
// is a common default) and continue the list through
// Members@odata.nextLink. DSP0268 collections are append-ordered
// oldest→newest, so a walk that ignores the link reads only the first page
// — and, for logs, precisely misses the newest records, the ones the
// rescue-room read exists for. followCollection is the one collection hop
// every walk shares.
//
// Error discipline mirrors the resource walks: a failed or undecodable
// FIRST page is the caller's error (op carries the failing resource); a
// later page that breaks degrades the read to what was gathered so far —
// partial data beats no data.
const (
	// maxCollectionPages bounds the nextLink chase against firmware bugs:
	// a link that loops or never ends must not wedge the caller.
	maxCollectionPages = 64
	// maxCollectionMembers bounds the accumulated links the same way.
	maxCollectionMembers = 8192
)

func followCollection(get func(string) ([]byte, error), op, url string) ([]odataLink, error) {
	var members []odataLink
	seen := map[string]bool{url: true}
	for hop := 0; ; hop++ {
		raw, err := get(url)
		if err != nil {
			if hop == 0 {
				return nil, err
			}
			return members, nil
		}
		var page struct {
			Members  []odataLink `json:"Members"`
			NextLink string      `json:"Members@odata.nextLink"`
		}
		if json.Unmarshal(raw, &page) != nil {
			if hop == 0 {
				return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
					Detail: url + ": collection undecodable"}
			}
			return members, nil
		}
		members = append(members, page.Members...)
		next, ok := nextLinkFor(page.NextLink)
		if !ok || seen[next] ||
			hop+1 >= maxCollectionPages || len(members) >= maxCollectionMembers {
			return members, nil
		}
		seen[next] = true
		url = next
	}
}

// nextLinkFor normalizes the nextLink's wire forms before the chase. The
// HTTP layer prepends the endpoint, so absolute-path references (the
// common form) pass through, full URLs must be reduced to path+query or
// they would double the host, and bare-relative references are unusable —
// the chase stops with what it has rather than guessing their base.
func nextLinkFor(next string) (string, bool) {
	next = strings.TrimSpace(next)
	switch {
	case next == "":
		return "", false
	case strings.HasPrefix(next, "https://"), strings.HasPrefix(next, "http://"):
		u, err := url.Parse(next)
		if err != nil || u.Path == "" {
			return "", false
		}
		if u.RawQuery != "" {
			return u.Path + "?" + u.RawQuery, true
		}
		return u.Path, true
	case strings.HasPrefix(next, "/"):
		return next, true
	default:
		return "", false
	}
}
