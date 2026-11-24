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
  nodeID      : Nat
  currentGen  : Nat
  clock       : Nat
  memRevoked  : List (Nat × Nat) -- (sessionID, until)
  diskRevoked : List (Nat × Nat) -- (sessionID, until) in authfile / Raft FSM
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

/-- auth.Service.Logout on node `targetNodeID` when cluster has `protocol` and `ttl`.
    If `protocol >= 24`, Raft replicates the revocation (with `revokedGrace`) to all nodes.
    If `protocol < 24` (`ErrClusterUpgrading`), only `memRevoked` on the target node is updated! -/
def logoutOnNode (st : ClusterState) (targetNodeID : Nat) (tok : SessionToken) (ttl : Nat) : ClusterState :=
  match st.nodes.find? (fun n => n.nodeID == targetNodeID) with
  | none => st
  | some tn =>
    if !validateSession tn tok then
      st
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
          let disk' := if revokedUntil > atTime then (tok.id, revokedUntil) :: pruneRaft n.diskRevoked atTime else pruneRaft n.diskRevoked atTime
          { n with memRevoked := mem', diskRevoked := disk' }
        { st with nodes := newNodes }
      else
        -- ErrClusterUpgrading: only this node's in-memory map is updated (auth.go:448-463)
        let newNodes := st.nodes.map fun n =>
          if n.nodeID == targetNodeID then
            { n with memRevoked := (tok.id, revokedUntil) :: pruneMem n.memRevoked tn.clock }
          else
            n
        { st with nodes := newNodes }

/-- Models time advancing to `newClock` and a subsequent Raft/authfile prune running at `atTime`
    (with Raft `revokedGrace` and authfile/mem `now < exp`). -/
def advanceAndPruneNode (n : NodeState) (newClock : Nat) : NodeState :=
  { n with
    clock := newClock
    memRevoked := n.memRevoked.filter (fun (_, exp) => newClock < exp)
    diskRevoked := n.diskRevoked.filter (fun (_, exp) => newClock < exp + revokedGrace) }

/-- Restarting a node clears volatile memory (`memRevoked`), keeping `diskRevoked` (Raft snapshot/log or authfile). -/
def restartNode (n : NodeState) : NodeState :=
  { n with memRevoked := [] }

/-- API Token model from internal/usecase/token/token.go. -/
structure APIToken where
  id        : Nat
  owner     : String -- "" for service token, non-empty for personal token
  revoked   : Bool
  hasExpiry : Bool
  expiry    : Nat
  deriving DecidableEq, Repr

/-- External auth user table (for modeling R-02 user deletion gap). -/
structure AuthSystem where
  activeUsers : List String
  tokens      : List APIToken
  deriving DecidableEq, Repr

/-- token.Service.Authenticate in origin/main (token.go:140-167):
    checks existence (not revoked) and expiry, does NOT check whether `owner` still exists in AuthSystem. -/
def authenticateToken (sys : AuthSystem) (tid : Nat) (now : Nat) : Bool :=
  match sys.tokens.find? (fun t => t.id == tid) with
  | none => false
  | some t =>
    !t.revoked && (!t.hasExpiry || now < t.expiry)

/-- Deleting an external user removes them from `activeUsers`, without revoking personal tokens in origin/main. -/
def deleteExternalUser (sys : AuthSystem) (user : String) : AuthSystem :=
  { sys with activeUsers := sys.activeUsers.filter (· != user) }

end DbBranchAuthz.Session
