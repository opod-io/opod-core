package fetch

// Whether the weights a fetch made present are signed (T32.23).
//
// A model-transparency signature (OpenSSF model signing) is a Sigstore bundle
// published beside the weights as `model.sig`, over a manifest of every file's
// digest. This answers the first half of "are these the bytes their publisher
// signed": is a signature published at all. It does NOT verify one — that
// needs the Sigstore trust root and the publisher's identity, and this release
// has neither on the node — so a published signature is reported as
// `unverified`, never as a pass.

import (
	"context"
	"fmt"

	"github.com/opod-io/opod-sdk/adminapi"
)

// SignatureOf says whether repo at opt.Revision publishes a signature beside
// its weights. One listing request; a Hub that cannot be asked is `unknown`,
// which says nothing either way.
func SignatureOf(ctx context.Context, repo string, opt Options) adminapi.FetchSignature {
	opt = opt.withDefaults()
	rev := snapshotRevision(opt.Revision)
	opt.Revision = rev
	_, files, err := listRevision(ctx, repo, opt)
	if err != nil {
		return adminapi.FetchSignature{State: adminapi.SignatureUnknown,
			Detail: fmt.Sprintf("could not list %s@%s to look for %s: %v", repo, rev, adminapi.SignatureFile, err)}
	}
	return signatureIn(repo, rev, files)
}

// signatureIn is the decision on a listing.
func signatureIn(repo, rev string, files []SnapshotFile) adminapi.FetchSignature {
	for _, f := range files {
		if f.Name == adminapi.SignatureFile {
			return adminapi.FetchSignature{State: adminapi.SignatureUnverified, File: f.Name,
				Detail: fmt.Sprintf("%s@%s publishes %s; this release does not verify model signatures, so nothing vouches for it yet", repo, rev, f.Name)}
		}
	}
	return adminapi.FetchSignature{State: adminapi.SignatureUnsigned,
		Detail: fmt.Sprintf("%s@%s publishes no %s", repo, rev, adminapi.SignatureFile)}
}
