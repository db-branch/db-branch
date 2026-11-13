package licensing

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

// AutoRenewSetting turns online renewal on (the default) or off (core scope).
const AutoRenewSetting = "license.auto_renew"

// Results of a renewal attempt.
const (
	ResultRenewed  = "renewed"
	ResultNotYet   = "not_yet"
	ResultInactive = "inactive"
	ResultError    = "error"
	// ResultUnknown: this node does not lead, so it does not know.
	ResultUnknown = "unknown"
)

// When the leader looks, and how long it waits after a failure.
const (
	CheckEvery      = 12 * time.Hour
	CheckJitter     = time.Hour
	FirstCheckAfter = time.Minute
	// RetryFirst is the wait after one failed attempt, RetryAgain after more.
	RetryFirst = time.Hour
	RetryAgain = 6 * time.Hour
	// MonthlyWindow and YearlyWindow: how long before Expires a key is renewed.
	MonthlyWindow = 7 * 24 * time.Hour
	YearlyWindow  = 30 * 24 * time.Hour
	// ManualEvery is the least time between two clicks on "Check now".
	ManualEvery = 30 * time.Second
	// tickEvery is how often the loop wakes up to see whether a check is due.
	tickEvery = 30 * time.Second
)

var (
	// ErrNoNewerKey: the licence service has no newer key yet (204).
	ErrNoNewerKey = errors.New("no newer key yet")
	// ErrSubscriptionInactive: the subscription is cancelled or unpaid (402).
	ErrSubscriptionInactive = errors.New("subscription is not active")
	// ErrKeyNotRecognised: the licence service does not know the key (400/401).
	ErrKeyNotRecognised = errors.New("the licence service does not recognise the key")
	// ErrInsecureURL: the renew URL is not https.
	ErrInsecureURL = errors.New("the renew URL must be https")

	// ErrAutoRenewOff: license.auto_renew is off; nothing contacts the network.
	ErrAutoRenewOff = errors.New("automatic renewal is turned off (license.auto_renew)")
	// ErrNotRenewable: the saved key has no subscription to renew online.
	ErrNotRenewable = errors.New("the saved licence key cannot be renewed online")
	// ErrNotLeader: only the cluster's leader contacts the licence service.
	ErrNotLeader = errors.New("this node is not the cluster leader")
	// ErrInFlight: another renewal request is running.
	ErrInFlight = errors.New("a renewal check is already running")
)

// RetryLaterError: the licence service asked to come back later (429).
type RetryLaterError struct{ After time.Duration }

func (e *RetryLaterError) Error() string {
	return fmt.Sprintf("the licence service asked to try again in %s", e.After.Round(time.Second))
}

// TooSoonError: "Check now" was used again within ManualEvery.
type TooSoonError struct{ RetryAfter time.Duration }

func (e *TooSoonError) Error() string {
	return fmt.Sprintf("checked a moment ago; try again in %ds", int(e.RetryAfter.Seconds())+1)
}

// RenewClient asks the licence service for the renewed key. clusterID is the
// cluster's anonymous id, or "" when there is none yet. It returns
// ErrNoNewerKey, ErrSubscriptionInactive or ErrKeyNotRecognised (wrapped,
// with the service's message) for the answers that mean those.
type RenewClient interface {
	Renew(ctx context.Context, renewURL, key, clusterID string) (string, error)
}

// CheckRenewURL refuses a renew URL that is not https. http is allowed only
// to 127.0.0.1 and localhost, for tests.
func CheckRenewURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: %q is not a URL", ErrInsecureURL, raw)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"):
		return nil
	}
	return fmt.Errorf("%w (got %s://)", ErrInsecureURL, u.Scheme)
}

// RenewalDue reports whether the key should be renewed now: it has a
// subscription and a renew URL, and is within its renewal window of Expires
// (7 days for monthly, 30 for yearly and anything else) or past it.
func RenewalDue(l license.License, now time.Time) bool {
	if l.Subscription == "" || l.RenewURL == "" {
		return false
	}
	window := YearlyWindow
	if l.Billing == license.Monthly {
		window = MonthlyWindow
	}
	return l.Expires.Sub(now) < window
}

// Accept decides whether next may replace cur: the same licence and
// customer, expiring later (or issued later, for a change in mid-period), and covering every module that is on and that
// cur names (or every module, when cur covers "*"). wanted are the modules
// that are on.
func Accept(cur, next license.License, wanted []string) error {
	switch {
	case next.ID != cur.ID:
		return fmt.Errorf("the renewed key is for licence %q, not %q", next.ID, cur.ID)
	case next.Customer != cur.Customer:
		return fmt.Errorf("the renewed key is for customer %q, not %q", next.Customer, cur.Customer)
	case !next.Expires.After(cur.Expires) && !next.IssuedAt.After(cur.IssuedAt):
		// A key that expires no later is a mid-period change of plan,
		// billing or node limit; it must then be issued later.
		return fmt.Errorf("the renewed key expires %s, not later than the current one (%s), and was not issued later", next.Expires.Format(time.DateOnly), cur.Expires.Format(time.DateOnly))
	}
	if slices.Contains(cur.Modules, "*") {
		if !slices.Contains(next.Modules, "*") {
			return errors.New("the renewed key no longer covers every module")
		}
		return nil
	}
	var missing []string
	for _, m := range wanted {
		if slices.Contains(cur.Modules, m) && !next.Covers(m) {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the renewed key does not cover the enabled module(s) %s", strings.Join(missing, ", "))
	}
	return nil
}

// RenewalStatus is what the leader knows about renewing the saved key.
type RenewalStatus struct {
	// Enabled: license.auto_renew is on.
	Enabled     bool       `json:"enabled"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
	// LastResult: renewed, not_yet, inactive, error, or unknown (this node
	// does not lead). Empty before the first attempt.
	LastResult  string     `json:"last_result,omitempty"`
	Message     string     `json:"message,omitempty"`
	NextAttempt *time.Time `json:"next_attempt,omitempty"`
}

// Renewer fetches the renewed key of a subscription before the saved one
// runs out. Run it on every node: it acts only on the Raft leader, so at
// most one request is in flight cluster-wide (apart from the instant of a
// leader change).
type Renewer struct {
	Service *Service
	Client  RenewClient
	// Leader reports whether this node leads the cluster.
	Leader func() bool
	// Notify tells admins (notify webhook / Slack). Optional.
	Notify func(ctx context.Context, event, level, subject, text string)
	Log    *slog.Logger
	// Cluster keeps the cluster's anonymous id, sent with every request so
	// that the licence service can notice a key used by several clusters.
	// Optional: without it no id is sent.
	Cluster domain.ClusterIDStore
	// Random is where a new cluster id comes from; nil: crypto/rand.
	Random io.Reader
	// Jitter returns the random part (±CheckJitter) added to the 12 hours.
	Jitter func() time.Duration

	busy sync.Mutex // held while a request is in flight

	mu         sync.Mutex
	wasLeader  bool
	next       time.Time
	last       time.Time
	lastResult string
	message    string
	fails      int
	lastManual time.Time
	told       map[string]bool
}

func (r *Renewer) now() time.Time { return r.Service.Clock() }

func (r *Renewer) jitter() time.Duration {
	if r.Jitter != nil {
		return r.Jitter()
	}
	return rand.N(2*CheckJitter) - CheckJitter
}

func (r *Renewer) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Leading reports whether this node leads.
func (r *Renewer) Leading() bool { return r.Leader() }

func (r *Renewer) enabled(ctx context.Context) (map[string]string, bool) {
	v, err := r.Service.Settings.Values(ctx, "core")
	return v, err != nil || v[AutoRenewSetting] != "false"
}

// Status is the leader's view; any other node says "unknown".
func (r *Renewer) Status(ctx context.Context) RenewalStatus {
	_, on := r.enabled(ctx)
	if !r.Leader() {
		return RenewalStatus{Enabled: on, LastResult: ResultUnknown}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := RenewalStatus{Enabled: on, LastResult: r.lastResult, Message: r.message}
	if !r.last.IsZero() {
		t := r.last
		st.LastAttempt = &t
	}
	if on && !r.next.IsZero() {
		t := r.next
		st.NextAttempt = &t
	}
	return st
}

// Run checks while this node leads, until ctx ends.
func (r *Renewer) Run(ctx context.Context) {
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(tickEvery):
		}
	}
}

// Tick does what is due now: schedules the first check after this node
// became leader, and runs a check when its time has come.
func (r *Renewer) Tick(ctx context.Context) {
	now := r.now()
	r.mu.Lock()
	if !r.Leader() {
		r.wasLeader, r.next = false, time.Time{}
		r.mu.Unlock()
		return
	}
	if !r.wasLeader {
		r.wasLeader, r.next = true, now.Add(FirstCheckAfter)
	}
	due := !now.Before(r.next)
	r.mu.Unlock()
	if !due {
		return
	}
	if err := r.check(ctx, false); err != nil && !errors.Is(err, ErrAutoRenewOff) && !errors.Is(err, ErrNotRenewable) && !errors.Is(err, ErrInFlight) {
		r.logger().Warn("checking the licence for renewal", "err", err)
	}
}

// CheckNow renews now whatever the key's expiry, for an admin. It refuses
// when automatic renewal is off, when the key has no subscription, and
// within ManualEvery of the last click.
func (r *Renewer) CheckNow(ctx context.Context) (RenewalStatus, error) {
	if !r.Leader() {
		return RenewalStatus{}, ErrNotLeader
	}
	r.mu.Lock()
	if wait := ManualEvery - r.now().Sub(r.lastManual); wait > 0 {
		r.mu.Unlock()
		return RenewalStatus{}, &TooSoonError{RetryAfter: wait}
	}
	r.lastManual = r.now()
	r.mu.Unlock()
	if err := r.check(ctx, true); err != nil {
		return RenewalStatus{}, err
	}
	return r.Status(ctx), nil
}

func (r *Renewer) schedule(d time.Duration) {
	r.mu.Lock()
	r.next = r.now().Add(d)
	r.mu.Unlock()
}

// check asks the licence service for a renewed key when one is due (always,
// when force). An error is returned only when no attempt was made or the
// attempt failed before the answer could be recorded; the outcome of an
// attempt is in Status.
func (r *Renewer) check(ctx context.Context, force bool) error {
	if !r.busy.TryLock() {
		return ErrInFlight
	}
	defer r.busy.Unlock()
	every := func() time.Duration { return CheckEvery + r.jitter() }
	v, on := r.enabled(ctx)
	if !on {
		r.schedule(every())
		return ErrAutoRenewOff
	}
	key := strings.TrimSpace(v[KeySetting])
	if r.Service.Root == nil {
		r.schedule(every())
		return ErrNoRoot
	}
	var cur license.License
	var err error
	if key != "" {
		cur, err = license.Verify(r.Service.Root, key)
	}
	switch {
	case key == "" || err == nil && (cur.Subscription == "" || cur.RenewURL == ""):
		r.schedule(every())
		return ErrNotRenewable
	case err != nil:
		r.schedule(every())
		return fmt.Errorf("the saved key does not verify: %w", err)
	case !force && !RenewalDue(cur, r.now()):
		r.schedule(every())
		return nil
	}
	result, msg, retry := r.renew(ctx, cur, key)
	now := r.now()
	r.mu.Lock()
	r.last, r.lastResult, r.message = now, result, msg
	if result == ResultError {
		r.fails++
	} else {
		r.fails = 0
	}
	wait := every()
	if result == ResultError {
		wait = RetryFirst
		if r.fails > 1 {
			wait = RetryAgain
		}
	}
	if retry > 0 {
		wait = max(retry, time.Minute)
	}
	r.next = now.Add(wait)
	r.mu.Unlock()
	log := r.logger().With("result", result, "message", msg)
	if result == ResultRenewed {
		log.Info("licence renewed")
	} else if retry > 0 {
		log.Info("licence service asked to retry later", "after", retry)
	} else {
		log.Warn("licence not renewed")
		r.tell(ctx, cur, result, msg)
	}
	return nil
}

// renew makes the request and, for a key it accepts, saves it.
func (r *Renewer) renew(ctx context.Context, cur license.License, key string) (result, msg string, retry time.Duration) {
	result, msg, retry = r.renewOnce(ctx, cur, key)
	return
}

func (r *Renewer) renewOnce(ctx context.Context, cur license.License, key string) (result, msg string, retry time.Duration) {
	if err := CheckRenewURL(cur.RenewURL); err != nil {
		return ResultError, err.Error(), 0
	}
	next, err := r.Client.Renew(ctx, cur.RenewURL, key, r.clusterID(ctx))
	var later *RetryLaterError
	switch {
	case errors.Is(err, ErrNoNewerKey):
		return ResultNotYet, "the licence service has no newer key yet", 0
	case errors.As(err, &later):
		return ResultNotYet, err.Error(), later.After
	case errors.Is(err, ErrSubscriptionInactive):
		return ResultInactive, clean(err.Error()), 0
	case err != nil:
		return ResultError, clean(err.Error()), 0
	}
	nl, err := license.Verify(r.Service.Root, strings.TrimSpace(next))
	if err != nil {
		return ResultError, "the key the licence service sent does not verify: " + err.Error(), 0
	}
	var wanted []string
	if r.Service.Modules != nil {
		specs, err := r.Service.Modules.List(ctx)
		if err != nil {
			return ResultError, "listing modules: " + err.Error(), 0
		}
		for _, m := range specs {
			if m.Wanted {
				wanted = append(wanted, m.Name)
			}
		}
	}
	if err := Accept(cur, nl, wanted); err != nil {
		return ResultError, err.Error() + "; keeping the current key", 0
	}
	if _, err := r.Service.SaveRenewed(ctx, next); err != nil {
		return ResultError, "saving the renewed key: " + err.Error(), 0
	}
	msg = "valid until " + nl.Expires.Format(time.DateOnly)
	if n := r.Service.Nodes(); nl.MaxNodes > 0 && n > nl.MaxNodes {
		msg += fmt.Sprintf("; the new key allows %d nodes but the cluster has %d", nl.MaxNodes, n)
	}
	return ResultRenewed, msg, 0
}

// clusterID is the cluster's anonymous id. A cluster that has none gets one
// now, drawn at random and recorded set-if-empty, so a renewal on two nodes
// at once (a leader change) ends with one id. When it cannot be had (no
// store, the cluster is not fully upgraded yet, no randomness) it is "" and
// the request goes without it.
func (r *Renewer) clusterID(ctx context.Context) string {
	if r.Cluster == nil {
		return ""
	}
	if id := r.Cluster.ClusterID(ctx); id != "" {
		return id
	}
	random := r.Random
	if random == nil {
		random = crand.Reader
	}
	fresh, err := domain.NewClusterID(random)
	if err == nil {
		fresh, err = r.Cluster.InitClusterID(ctx, fresh)
	}
	if err != nil {
		r.logger().Debug("no cluster id for the renewal request", "err", err)
		return ""
	}
	return fresh
}

// tell notifies admins, once per licence expiry, that a subscription is
// inactive, or that the key is in its grace period and renewal keeps failing.
func (r *Renewer) tell(ctx context.Context, cur license.License, result, msg string) {
	if r.Notify == nil {
		return
	}
	once := func(event string) bool {
		k := event + "/" + cur.ID + "/" + cur.Expires.Format(time.RFC3339)
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.told == nil {
			r.told = map[string]bool{}
		}
		if r.told[k] {
			return false
		}
		r.told[k] = true
		return true
	}
	subject := cur.ID
	if result == ResultInactive && once("license.renewal_inactive") {
		r.Notify(ctx, "license.renewal_inactive", "warning", subject,
			fmt.Sprintf("The subscription of licence %s is not active (%s). The paid modules stop working after %s unless it is renewed.", cur.ID, msg, cur.Expires.AddDate(0, 0, cur.GraceDays).Format(time.DateOnly)))
	}
	if cur.State(r.now(), r.Service.Nodes()) == license.Grace && once("license.grace") {
		r.Notify(ctx, "license.grace", "error", subject,
			fmt.Sprintf("Licence %s expired on %s and is in its grace period until %s, and automatic renewal is failing (%s). Paste a new key in Settings, Modules, License.",
				cur.ID, cur.Expires.Format(time.DateOnly), cur.Expires.AddDate(0, 0, cur.GraceDays).Format(time.DateOnly), msg))
	}
}

// clean shortens and flattens a message that came from the network.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if rs := []rune(s); len(rs) > 300 {
		s = string(rs[:300]) + "…"
	}
	return strings.TrimSpace(s)
}
