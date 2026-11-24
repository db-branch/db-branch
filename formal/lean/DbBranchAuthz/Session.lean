namespace DbBranchAuthz.Session

/-- Cluster-wide / single-node session & revocation model matching:
    - internal/usecase/auth/auth.go
    - internal/infrastructure/raftstore/fsm.go (Protocol 24, revokedGrace = 300s)
    - internal/infrastructure/authfile/revocations.go
    - internal/usecase/token/token.go -/

def revokedGrace : Nat := 300

structure SessionToken where
  id     : Nat
  user   : String
  gen    : Nat
  expiry : Nat -- unix seconds
  deriving DecidableEq, Repr

structure NodeState where
  nodeID         : Nat
  currentGen     : Nat
  clock          : Nat
  memRevoked     : List (Nat × Nat) -- (sessionID, until)
  pendingRevoked : List (Nat × Nat) -- (sessionID, until) queued during ErrClusterUpgrading (F-09)
  diskRevoked    : List (Nat × Nat) -- (sessionID, until) in authfile / Raft FSM
  deriving DecidableEq, Repr

structure ClusterState where
  protocol : Nat -- < 24 means mixed upgrading cluster (ErrClusterUpgrading)
  nodes    : List NodeState
  deriving DecidableEq, Repr

def isRevokedIn (id : Nat) (entries : List (Nat × Nat)) : Bool :=
  entries.any (fun (sid, _) => sid == id)

/-- auth.Service.Session validation on a given node at node.clock (auth.go:396-418). -/
def validateSession (n : NodeState) (tok : SessionToken) : Bool :=
  let revoked := isRevokedIn tok.id n.memRevoked || isRevokedIn tok.id n.diskRevoked
  tok.gen == n.currentGen && n.clock < tok.expiry && !revoked

/-- auth.Service.PendingSession validation for POST /v1/auth/logout retry (auth.go:411-418, server.go:463). -/
def validatePendingSession (n : NodeState) (tok : SessionToken) : Bool :=
  tok.gen == n.currentGen && n.clock < tok.expiry &&
  isRevokedIn tok.id n.pendingRevoked && !isRevokedIn tok.id n.diskRevoked

/-- auth.Service.Logout on node `targetNodeID` when cluster has `protocol` and `ttl` (post-F-09).
    Returns `(newClusterState, httpLogoutOK)`.
    Accepts either an active session (`validateSession`) or a pending revocation retry (`validatePendingSession`).
    If `protocol >= 24`, Raft replicates the revocation (with `revokedGrace`) to all nodes and returns `httpLogoutOK = true` (204).
    If `protocol < 24` (`ErrClusterUpgrading`), only `memRevoked` and `pendingRevoked` on the target node are updated,
    and HTTP `POST /v1/auth/logout` returns `503 cluster_upgrading` (`httpLogoutOK = false`), preserving the cookie for retry/flush! -/
def logoutOnNode (st : ClusterState) (targetNodeID : Nat) (tok : SessionToken) (ttl : Nat) : ClusterState × Bool :=
  match st.nodes.find? (fun n => n.nodeID == targetNodeID) with
  | none => (st, true)
  | some tn =>
    let canLogout := validateSession tn tok || validatePendingSession tn tok
    if !canLogout then
      (st, true)
    else
      let revokedUntil := max tok.expiry (tn.clock + ttl)
      let pruneMem (xs : List (Nat × Nat)) (now : Nat) :=
        xs.filter (fun (_, exp) => now < exp)
      let pruneRaft (xs : List (Nat × Nat)) (atTime : Nat) :=
        xs.filter (fun (_, exp) => atTime < exp + revokedGrace)
      if st.protocol >= 24 then
        -- Raft replicates RevokeSession to every node's persistent FSM state
        let atTime := tn.clock
        let newNodes := st.nodes.map fun n =>
          let mem' := if n.nodeID == targetNodeID then (tok.id, revokedUntil) :: pruneMem n.memRevoked atTime else n.memRevoked
          let pend' := if n.nodeID == targetNodeID then n.pendingRevoked.filter (fun (sid, _) => sid != tok.id) else n.pendingRevoked
          let disk' := if revokedUntil > atTime then (tok.id, revokedUntil) :: pruneRaft n.diskRevoked atTime else pruneRaft n.diskRevoked atTime
          { n with memRevoked := mem', pendingRevoked := pend', diskRevoked := disk' }
        ({ st with nodes := newNodes }, true)
      else
        -- ErrClusterUpgrading: local memory + pendingRevoke queue updated, HTTP returns 503 cluster_upgrading
        let newNodes := st.nodes.map fun n =>
          if n.nodeID == targetNodeID then
            { n with
              memRevoked := (tok.id, revokedUntil) :: pruneMem n.memRevoked tn.clock
              pendingRevoked := (tok.id, revokedUntil) :: pruneMem n.pendingRevoked tn.clock }
          else
            n
        ({ st with nodes := newNodes }, false)

/-- Models the cluster finishing an upgrade to `newProtocol`. -/
def upgradeCluster (st : ClusterState) (newProtocol : Nat) : ClusterState :=
  { st with protocol := newProtocol }

/-- auth.Service.FlushRevocations invoked on `flusherNodeID` (e.g. by healthLoop or logout retry)
    after the cluster reaches `protocol >= 24`. Replicates all unexpired pending revocations to Raft. -/
def flushPendingRevocations (st : ClusterState) (flusherNodeID : Nat) : ClusterState :=
  if st.protocol < 24 then
    st
  else
    match st.nodes.find? (fun n => n.nodeID == flusherNodeID) with
    | none => st
    | some fn =>
      let active := fn.pendingRevoked.filter (fun (_, exp) => fn.clock < exp)
      let pruneRaft (xs : List (Nat × Nat)) (atTime : Nat) :=
        xs.filter (fun (_, exp) => atTime < exp + revokedGrace)
      let newNodes := st.nodes.map fun n =>
        let pend' := if n.nodeID == flusherNodeID then [] else n.pendingRevoked
        let disk' := active ++ pruneRaft n.diskRevoked fn.clock
        { n with pendingRevoked := pend', diskRevoked := disk' }
      { st with nodes := newNodes }

/-- Models time advancing to `newClock` and a subsequent Raft/authfile prune running at `atTime`
    (with Raft `revokedGrace` and authfile/mem `now < exp`). -/
def advanceAndPruneNode (n : NodeState) (newClock : Nat) : NodeState :=
  { n with
    clock := newClock
    memRevoked := n.memRevoked.filter (fun (_, exp) => newClock < exp)
    pendingRevoked := n.pendingRevoked.filter (fun (_, exp) => newClock < exp)
    diskRevoked := n.diskRevoked.filter (fun (_, exp) => newClock < exp + revokedGrace) }

/-- Restarting a node clears volatile memory (`memRevoked` and `pendingRevoked`), keeping `diskRevoked` (Raft snapshot/log or authfile). -/
def restartNode (n : NodeState) : NodeState :=
  { n with memRevoked := [], pendingRevoked := [] }

/-- API Token model from internal/usecase/token/token.go. -/
structure APIToken where
  id        : Nat
  owner     : String -- "" for service token, non-empty for personal token
  revoked   : Bool
  hasExpiry : Bool
  expiry    : Nat
  deriving DecidableEq, Repr

/-- External auth user table and token store (post-R-02). -/
structure AuthSystem where
  activeUsers : List String
  tokens      : List APIToken
  deriving DecidableEq, Repr

/-- token.Service.Authenticate in internal/usecase/token/token.go:212-240:
    checks existence (not revoked) and expiry. -/
def authenticateToken (sys : AuthSystem) (tid : Nat) (now : Nat) : Bool :=
  match sys.tokens.find? (fun t => t.id == tid) with
  | none => false
  | some t =>
    !t.revoked && (!t.hasExpiry || now < t.expiry)

/-- token.Service.RevokeByOwner (token.go:158-170): deletes all personal tokens
    owned by `owner`, leaving service tokens (`owner == ""`) untouched. -/
def revokeTokensByOwner (tokens : List APIToken) (owner : String) : List APIToken :=
  if owner == "" then
    tokens
  else
    tokens.filter (fun t => t.owner != owner)

/-- R-02: deleting an external user removes them from `activeUsers` and
    immediately revokes all of their personal API tokens via `RevokeByOwner`. -/
def deleteExternalUser (sys : AuthSystem) (user : String) : AuthSystem :=
  { activeUsers := sys.activeUsers.filter (· != user)
  , tokens := revokeTokensByOwner sys.tokens user }

end DbBranchAuthz.Session
