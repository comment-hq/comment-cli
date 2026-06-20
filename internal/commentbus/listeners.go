package commentbus

import (
	"sync"
	"time"
)

// listenerRegistry is the daemon's in-memory view of "who is listening for
// messages on a handle". It unifies two related concerns behind one mutex:
//
//   - pull-waiters: an in-flight `messages.wait --rewake` call (asyncRewake). A
//     Claude Code session pulls its own Comment.io messages instead of being
//     typed into by bmux, so while such a wait is blocked the daemon skips the
//     bmux keystroke for that session and leaves the ready message in the local
//     queue for the waiter to claim. Keyed by session identity
//     (profile+session_id+generation) or by profile alone when no session
//     triple is present (impromptu free-handle listen). Reference-counted:
//     register on wait entry, deregister with defer on return, so a returned or
//     dropped wait removes the waiter.
//
//   - listen claims: an impromptu attach to a free (non-daemon-managed) handle.
//     At most one claim per handle, no takeover. Created by listen.claim and
//     removed by listen.release; also cleared when the daemon restarts (the
//     registry is in-memory).
type listenerRegistry struct {
	mu          sync.Mutex
	pullWaiters map[string]int
	claims      map[string]listenClaim
}

type listenClaim struct {
	Handle    string
	ClaimedBy string
	ClaimedAt time.Time
}

func newListenerRegistry() *listenerRegistry {
	return &listenerRegistry{
		pullWaiters: map[string]int{},
		claims:      map[string]listenClaim{},
	}
}

// listenerWaiterKey derives the registry key for a pull-waiter. A full session
// triple is matched exactly; an empty triple collapses to the profile, which is
// the impromptu-listen case (bare Claude with COMMENT_IO_PROFILE set and no
// managed-session env).
func listenerWaiterKey(profile, sessionID, generation string) string {
	if sessionID != "" && generation != "" {
		return "session:" + profile + ":" + sessionID + ":" + generation
	}
	return "profile:" + profile
}

// registerPullWaiter records an in-flight rewake wait and returns a function
// that removes it. The returned function is idempotent (safe to call once via
// defer); the count never drops below zero.
func (r *listenerRegistry) registerPullWaiter(profile, sessionID, generation string) func() {
	key := listenerWaiterKey(profile, sessionID, generation)
	r.mu.Lock()
	r.pullWaiters[key]++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.pullWaiters[key] <= 1 {
				delete(r.pullWaiters, key)
			} else {
				r.pullWaiters[key]--
			}
			r.mu.Unlock()
		})
	}
}

// hasPullWaiter reports whether a rewake wait is currently in flight for the
// given identity. A non-empty session triple matches a session-scoped waiter
// exactly; an empty triple matches a profile-scoped waiter.
func (r *listenerRegistry) hasPullWaiter(profile, sessionID, generation string) bool {
	if r == nil || profile == "" {
		return false
	}
	key := listenerWaiterKey(profile, sessionID, generation)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pullWaiters[key] > 0
}

// claimListen registers an impromptu listen claim for handle. It returns the
// granted claim and true, or the existing claim and false when the handle is
// already claimed (HANDLE_BUSY — there is no takeover).
func (r *listenerRegistry) claimListen(handle, claimedBy string, now time.Time) (listenClaim, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.claims[handle]; ok {
		return existing, false
	}
	claim := listenClaim{Handle: handle, ClaimedBy: claimedBy, ClaimedAt: now}
	r.claims[handle] = claim
	return claim, true
}

// releaseListen force-removes any listen claim for handle (no session check). It
// returns the removed claim and true, or a zero claim and false when the handle
// was not claimed. Used by registry unit tests and the force/cleanup path.
func (r *listenerRegistry) releaseListen(handle string) (listenClaim, bool) {
	claim, released, _ := r.releaseListenScoped(handle, "", true)
	return claim, released
}

// releaseListenScoped enforces the no-takeover rule: a live claim is released
// only when force is set, or when session matches the claim's ClaimedBy. It
// returns (claim, released, mismatch); mismatch==true means a live claim was
// held by a different session and no force was given, so nothing was released
// and the caller must refuse (so a second session can't steal a handle by
// releasing it). An unclaimed handle returns (zero, false, false).
func (r *listenerRegistry) releaseListenScoped(handle, session string, force bool) (listenClaim, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claim, ok := r.claims[handle]
	if !ok {
		return listenClaim{}, false, false
	}
	// Release when forced, when the claim is anonymous (no owning session to
	// protect — e.g. a CLI `listen claim` with no --session), or when the caller's
	// session matches the owner. A session-owned claim with a missing or mismatched
	// session is refused so a second session can't steal a handle by releasing it.
	if force || claim.ClaimedBy == "" || (session != "" && session == claim.ClaimedBy) {
		delete(r.claims, handle)
		return claim, true, false
	}
	return claim, false, true
}

// dropClaimsForManaged removes any impromptu listen claims for handles that are
// now daemon-managed. A config reload that promotes a free handle to a managed
// bot must invalidate any in-flight impromptu claim on it — otherwise the
// impromptu listener and the managed owner path would both deliver the handle's
// messages (double-delivery, breaking single-listener). Returns the handles whose
// claims were dropped (for logging).
func (r *listenerRegistry) dropClaimsForManaged(managed map[string]struct{}) []string {
	if r == nil || len(managed) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var dropped []string
	for handle := range r.claims {
		if _, ok := managed[handle]; ok {
			delete(r.claims, handle)
			dropped = append(dropped, handle)
		}
	}
	return dropped
}

// claimFor returns the current listen claim for handle, if any.
func (r *listenerRegistry) claimFor(handle string) (listenClaim, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claim, ok := r.claims[handle]
	return claim, ok
}
