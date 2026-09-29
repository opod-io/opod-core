package fetch

// A Hub that refuses a fetch says why, by name.
//
// A gated repository — most of what an enterprise asks for first — answers an
// anonymous caller 401, and so does a private one, and so does one that does
// not exist: the Hub does not tell a stranger which. That used to reach the
// operator as `GET <url> → 401 Unauthorized`, at the end of a log, which says
// what the Hub did and nothing about what to do. The three cases below are the
// ones an operator can act on, and each has a different remedy, so each has a
// name a caller (a person, or a program that runs this binary as a Job) can
// match on without parsing a sentence.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// The reasons. They are part of what `opod fetch` prints and therefore a
// contract: a name is never reused for another meaning.
const (
	// HubTokenMissing: the Hub wants a credential and none was sent.
	HubTokenMissing = "hub-token-missing"
	// HubTokenRefused: a token was sent and the Hub does not accept it —
	// mistyped, expired or revoked.
	HubTokenRefused = "hub-token-refused"
	// HubAccessDenied: the token is good and its account may not read this
	// repository — a gated repository's terms were never accepted, or a
	// fine-grained token was not given this repository.
	HubAccessDenied = "hub-access-denied"
)

// HubError is a fetch the Hub refused for a reason that has a name.
type HubError struct {
	Reason string // one of the Hub* constants
	Repo   string
	Host   string // who answered: huggingface.co, or the mirror HF_ENDPOINT names
	Status string // the HTTP status line, kept because a mirror may mean something of its own by it
	Code   string // the Hub's X-Error-Code (GatedRepo, RepoNotFound, …); a mirror may send none
}

func (e *HubError) Error() string {
	said := e.Status
	if e.Code != "" {
		said += ", " + e.Code
	}
	switch e.Reason {
	case HubTokenMissing:
		return fmt.Sprintf("%s: %s answered %s for %s and no Hugging Face token was sent — the repository is gated or private (the Hub answers an anonymous caller the same way when it does not exist, so check the name too). Set HF_TOKEN to a token with read access to it",
			e.Reason, e.Host, said, e.Repo)
	case HubTokenRefused:
		return fmt.Sprintf("%s: %s answered %s for %s although a Hugging Face token was sent — the token is mistyped, expired or revoked. Replace HF_TOKEN with a current one",
			e.Reason, e.Host, said, e.Repo)
	default:
		return fmt.Sprintf("%s: %s answered %s for %s — the token is valid and its account may not read this repository. Accept the repository's terms on its Hub page as that account, or give a fine-grained token read access to it",
			e.Reason, e.Host, said, e.Repo)
	}
}

// RefusalReason is the name of the reason the Hub refused, or "" when err is
// not such a refusal.
func RefusalReason(err error) string {
	var he *HubError
	if errors.As(err, &he) {
		return he.Reason
	}
	return ""
}

// hubRefusal reads a response as a refusal of the caller's credentials, or
// returns nil when it is anything else (a 404 with a token is a wrong name, a
// 5xx is the Hub's own trouble — neither is about the token).
//
// The decision is made on the status and on whether a token was sent, never on
// X-Error-Code alone: a mirror need not send the header, and the status is the
// part every server agrees on.
func hubRefusal(resp *http.Response, opt Options, repo, rawURL string) error {
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return nil
	}
	e := &HubError{Repo: repo, Host: rawURL, Status: resp.Status, Code: resp.Header.Get("X-Error-Code")}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		e.Host = u.Host
	}
	switch {
	case opt.Token == "":
		e.Reason = HubTokenMissing
	case resp.StatusCode == http.StatusUnauthorized:
		e.Reason = HubTokenRefused
	default:
		e.Reason = HubAccessDenied
	}
	return e
}
