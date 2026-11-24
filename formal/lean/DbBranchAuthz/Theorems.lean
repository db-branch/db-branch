import DbBranchAuthz.Types
import DbBranchAuthz.Model
import DbBranchAuthz.Session
import DbBranchAuthz.License

namespace DbBranchAuthz.Theorems

open DbBranchAuthz
open DbBranchAuthz.Session
open DbBranchAuthz.License

/-! ## Property 1: Default Deny (既定拒否) -/

theorem prop1_unauthenticated_http_denied (req : AuthRequest)
    (hChan : req.channel = .publicHTTP)
    (hAuth : req.authKind = .none)
    (hNotPre : req.endpoint.isPreMuxPublic = false)
    (hNotLogin : req.endpoint.isLogin = false)
    (hNotPubExt : req.endpoint.isPublicExt = false)
    (hNotAnon : req.endpoint.anonymousAllowed = false) :
    evalRequest req = Decision.deny .unauthenticated := by
  simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hNotAnon]

theorem prop1_anonymous_principal_only_allowlist (isTok : Bool) (ep : Endpoint) (rel : OwnerRel)
    (hAllow : (ownershipAuthorize .anonymous isTok ep rel).1 = true) :
    ep.action = "branch.create" ∨
    ep.action = "operation.read" ∨
    ep.action = "base.public" ∨
    ((ep.action = "branch.read" ∨ ep.action = "branch.connect") ∧ ep.hasBranchName = true ∧ rel = .openBranch) := by
  cases ep <;> cases rel <;>
    simp [ownershipAuthorize, canonicalAction, Endpoint.action, Endpoint.hasBranchName] at hAllow ⊢

theorem prop1_anonymous_http_only_allowlist (req : AuthRequest)
    (hChan : req.channel = .publicHTTP)
    (hAuth : req.authKind = .none)
    (hNotPre : req.endpoint.isPreMuxPublic = false)
    (hNotLogin : req.endpoint.isLogin = false)
    (hNotPubExt : req.endpoint.isPublicExt = false)
    (hAllowed : (evalRequest req).allowed = true) :
    req.endpoint = .branchCreate ∨
    req.endpoint = .basePublic ∨
    req.endpoint = .operationGet ∨
    ((req.endpoint = .branchGet ∨ req.endpoint = .branchDSN) ∧ req.ownerRel = .openBranch) := by
  have hAnon : req.endpoint.anonymousAllowed = true := by
    cases h : req.endpoint.anonymousAllowed
    · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, h, Decision.deny] at hAllowed
    · rfl
  cases hEp : req.endpoint <;> (rw [hEp] at hAnon; simp [Endpoint.anonymousAllowed] at hAnon)
  all_goals
    cases hRel : req.ownerRel <;>
      simp [evalRequest, evalGuarded, hChan, hAuth, hEp, hRel,
        Endpoint.isPreMuxPublic, Endpoint.isLogin, Endpoint.isPublicExt,
        Endpoint.anonymousAllowed, Endpoint.isSocketOnly, Endpoint.action,
        Endpoint.hasBranchName, canonicalAction, tokenScopedAuthorize,
        ownershipAuthorize, Decision.allow, Decision.deny] at hAllowed ⊢

theorem prop1_missing_forward_headers_denied (req : AuthRequest)
    (hChan : req.channel = .nodeForward)
    (hAuth : req.authKind = .missingForwardHeaders) :
    evalRequest req = Decision.deny .forbidden := by
  simp [evalRequest, hChan, hAuth]

theorem prop1_malformed_forward_denied (req : AuthRequest)
    (hChan : req.channel = .nodeForward)
    (hAuth : req.authKind = .malformedForward)
    (hNotPre : req.endpoint.isPreMuxPublic = false)
    (hNotLogin : req.endpoint.isLogin = false)
    (hNotPubExt : req.endpoint.isPublicExt = false) :
    (evalRequest req).allowed = false := by
  cases hSock : req.endpoint.isSocketOnly <;>
    simp [evalRequest, evalGuarded, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hSock, Decision.deny]

theorem prop1_bearer_local_id_rejected (req : AuthRequest)
    (hChan : req.channel = .publicHTTP)
    (hAuth : req.authKind = .bearerToken)
    (hLocal : req.role = .localPrincipal)
    (hNotPre : req.endpoint.isPreMuxPublic = false)
    (hNotLogin : req.endpoint.isLogin = false) :
    evalRequest req = Decision.deny .unauthenticated := by
  simp [evalRequest, hChan, hAuth, hLocal, hNotPre, hNotLogin]

/-- Counterexample (OSS without Ext.Authorizer): an authenticated user with
    empty/viewer roles is allowed cluster-wide admin operations like `cluster.remove`. -/
theorem prop1_oss_nonadmin_allows_admin_action_counterexample :
    ∃ req : AuthRequest,
      req.channel = .publicHTTP ∧
      req.role = .viewer ∧
      req.endpoint = .clusterRemove ∧
      (evalRequest req).allowed = true := by
  refine ⟨⟨.publicHTTP, .bearerToken, .viewer, false, .valid, .clusterRemove, .other⟩, ?_⟩
  decide

/-! ## Property 2: No Privilege Escalation / `branches` Scope Whitelist -/

theorem prop2_branches_token_cannot_create_personal_token (req : AuthRequest)
    (hRole : req.role = .scopeBranches)
    (hEp : req.endpoint = .accountTokenCreate)
    (hNotSocket : req.channel ≠ .socket) :
    (evalRequest req).allowed = false := by
  cases hChan : req.channel
  · contradiction
  · cases hAuth : req.authKind <;>
      simp [evalRequest, evalGuarded, hChan, hAuth, hEp, hRole, Endpoint.isPreMuxPublic,
        Endpoint.isLogin, Endpoint.isPublicExt, Endpoint.isSocketOnly, Endpoint.action,
        tokenScopedAuthorize, isBranchAction, isDotReadAction, isSecretRead, Decision.deny]
  · cases hAuth : req.authKind <;> cases hSess : req.sessState <;>
      simp [evalRequest, evalGuarded, hChan, hAuth, hSess, hEp, hRole, Endpoint.isPreMuxPublic,
        Endpoint.isLogin, Endpoint.isPublicExt, Endpoint.anonymousAllowed,
        Endpoint.allowedBeforePasswordChange, Endpoint.isGet, Endpoint.action,
        tokenScopedAuthorize, isBranchAction, isDotReadAction, isSecretRead, Decision.deny]

theorem prop2_branches_token_secret_reads_denied (ep : Endpoint)
    (hSecret : isSecretRead ep.action = true) :
    tokenScopedAuthorize .scopeBranches ep.action = false := by
  cases ep <;> simp [Endpoint.action, isSecretRead, tokenScopedAuthorize, isBranchAction] at hSecret ⊢

/-- Counterexample (Bug Candidate #2a / R-02): on origin/main, `token.Scoped` uses
    a 5-entry blacklist, so `scope:branches` leaks `cluster.read`, `license.read`,
    `module.read`, and `ext.<mod>.read`. -/
theorem prop2_branches_token_blacklist_leak_counterexample :
    (evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .clusterRead, .other⟩).allowed = true ∧
    (evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .licenseRead, .other⟩).allowed = true ∧
    (evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .moduleRead, .other⟩).allowed = true ∧
    (evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .extPrivateRead, .other⟩).allowed = true := by
  decide

/-- Counterexample (Bug Candidate #2b): modeling `viewerReadsOnly` (a module authorizer
    that rejects `role == .viewer` on non-read actions), a viewer's personal token
    drops the `"viewer"` role (carrying only `scope:branches`), escalating privilege
    on `branch.delete`! -/
def viewerReadsOnly (r : Role) (action : String) : Bool :=
  if r == .viewer then
    isDotReadAction action || action == "auth.login" || action == "auth.session" ||
    action == "auth.logout" || action == "auth.password" || action == "auth.reset" ||
    action == "capability.read"
  else
    true

theorem prop2_personal_token_role_drop_counterexample :
    viewerReadsOnly .viewer Endpoint.branchDelete.action = false ∧
    (viewerReadsOnly .scopeBranches Endpoint.branchDelete.action = true ∧
      (evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, false, .valid, .branchDelete, .ownByID⟩).allowed = true) := by
  decide

/-- Under the R-02 whitelist (`tokenScopedWhitelist`), `scope:branches` rejects
    cluster/license/module/ext reads as well as secret reads. -/
theorem prop2_whitelist_blocks_nonbranch_reads :
    tokenScopedWhitelist .scopeBranches Endpoint.clusterRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.licenseRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.moduleRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.extPrivateRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.settingRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.tokenRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.backupRead.action = false ∧
    tokenScopedWhitelist .scopeBranches Endpoint.accountTokenRead.action = false := by
  decide

/-! ## Property 3: Decision 13 Action Aliases (W-22 `legacyActionAuthorizer` + whitelist) -/

theorem prop3_alias_eval_equiv (r : Role) (isTok : Bool) (rel : OwnerRel) :
    evalActionWithWhitelist r isTok "branch.checkpoint_delete" rel =
      evalActionWithWhitelist r isTok "branch.checkpoint.delete" rel ∧
    evalActionWithWhitelist r isTok "base.snapshot_delete" rel =
      evalActionWithWhitelist r isTok "base.snapshot.delete" rel := by
  simp [evalActionWithWhitelist, canonicalAction]

theorem prop3_w22_legacy_authorizer_symmetric_aliases (baseAllow : String → Bool) :
    legacyActionAuthorize baseAllow "branch.checkpoint.delete" =
      legacyActionAuthorize baseAllow "branch.checkpoint_delete" ∧
    legacyActionAuthorize baseAllow "base.snapshot.delete" =
      legacyActionAuthorize baseAllow "base.snapshot_delete" ∧
    legacyActionAuthorize baseAllow "branch.pause" =
      (baseAllow "branch.pause" || baseAllow "branch.update") ∧
    legacyActionAuthorize baseAllow "branch.resume" =
      (baseAllow "branch.resume" || baseAllow "branch.update") := by
  simp [legacyActionAuthorize, legacyAliases, Bool.or_comm]

/-! ## Property 4: `branch.read` Visibility, Immutable ID & `Owner == ""` Protection -/

theorem prop4_branch_read_visibility (r : Role) (isTok : Bool) (rel : OwnerRel)
    (hNonAdmin : isAdmin r = false)
    (hNonAnon : r ≠ .anonymous)
    (hMigrated : rel ≠ .ownByLegacyName) :
    canReadBranch r isTok rel = true ↔
      (rel = .ownByID ∨ rel = .shared ∨ rel = .openBranch) := by
  cases rel <;> simp [canReadBranch, canManageBranch, ownsBranch, hNonAdmin, hNonAnon] at hMigrated ⊢

theorem prop4_no_owner_protected (r : Role) (isTok : Bool)
    (hNonAdmin : isAdmin r = false) :
    canManageBranch r isTok .noOwner = false ∧
    canReadBranch r isTok .noOwner = false := by
  simp [canReadBranch, canManageBranch, hNonAdmin]

theorem prop4_display_name_collision_safe_for_migrated (r : Role) (isTok : Bool)
    (hNonAdmin : isAdmin r = false) :
    canManageBranch r isTok .other = false ∧
    canReadBranch r isTok .other = false := by
  cases r <;> simp [canReadBranch, canManageBranch, ownsBranch, isAdmin] at hNonAdmin ⊢

/-- Counterexample (Bug Candidate #3): for legacy unmigrated branches (`OwnerName == ""`),
    a non-token external user (`isTokenID = false`) whose display name collides with
    `b.Owner` can read and manage the branch! -/
theorem prop4_legacy_unmigrated_name_collision_counterexample :
    canReadBranch .viewer false .ownByLegacyName = true ∧
    canManageBranch .viewer false .ownByLegacyName = true ∧
    (evalRequest ⟨.publicHTTP, .bearerToken, .viewer, false, .valid, .branchDelete, .ownByLegacyName⟩).allowed = true := by
  decide

/-! ## Property 5: `socket_only` Operations Must Reject Non-Socket Channels -/

private theorem evalGuarded_socketOnly_false (isLoc isBuiltIn validID hasSess : Bool) (r : Role) (isTok : Bool)
    (ep : Endpoint) (rel : OwnerRel) (hSock : ep.isSocketOnly = true) :
    (evalGuarded false isLoc isBuiltIn validID hasSess r isTok ep rel).allowed = false := by
  unfold evalGuarded
  split
  · rfl
  · split
    · rfl
    · dsimp only
      split
      · rfl
      · split
        · rfl
        · split
          · rfl
          · split
            · rfl
            · split
              · rfl
              · split
                · rfl
                · simp [hSock, Decision.deny]

theorem prop5_socket_only_public_http_denied (req : AuthRequest)
    (hChan : req.channel = .publicHTTP)
    (hSockOnly : req.endpoint.isSocketOnly = true) :
    (evalRequest req).allowed = false := by
  have hNotPre : req.endpoint.isPreMuxPublic = false := by
    cases hEp : req.endpoint <;> simp_all [Endpoint.isSocketOnly, Endpoint.isPreMuxPublic]
  have hNotLogin : req.endpoint.isLogin = false := by
    cases hEp : req.endpoint <;> simp_all [Endpoint.isSocketOnly, Endpoint.isLogin]
  have hNotPubExt : req.endpoint.isPublicExt = false := by
    cases hEp : req.endpoint <;> simp_all [Endpoint.isSocketOnly, Endpoint.isPublicExt]
  have hNotAnon : req.endpoint.anonymousAllowed = false := by
    cases hEp : req.endpoint <;> simp_all [Endpoint.isSocketOnly, Endpoint.anonymousAllowed]
  have hNotMust : req.endpoint.allowedBeforePasswordChange = false := by
    cases hEp : req.endpoint <;> simp_all [Endpoint.isSocketOnly, Endpoint.allowedBeforePasswordChange]
  have hNotGet : req.endpoint.isGet = false := by
    cases hEp : req.endpoint <;> simp_all [Endpoint.isSocketOnly, Endpoint.isGet]
  cases hAuth : req.authKind
  · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hNotAnon, Decision.deny]
  · cases hSess : req.sessState <;>
      cases hMask : (req.endpoint == .baseMask) <;>
      simp [evalRequest, hChan, hAuth, hSess, hNotPre, hNotLogin, hNotPubExt, hNotMust, hNotGet, hMask,
        Decision.deny, evalGuarded_socketOnly_false _ _ _ _ _ _ _ _ hSockOnly]
  · cases hLocRole : (req.role == .localPrincipal) <;>
      cases hMask : (req.endpoint == .baseMask) <;>
      simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hLocRole, hMask, Decision.deny,
        evalGuarded_socketOnly_false _ _ _ _ _ _ _ _ hSockOnly]
  · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, Decision.deny]
  · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, Decision.deny]

/-- On post-R-05 origin/main (`b025831`), `nodeForward` ALWAYS rejects `socket_only` operations. -/
theorem prop5_socket_only_node_forward_denied (req : AuthRequest)
    (hChan : req.channel = .nodeForward)
    (hSockOnly : req.endpoint.isSocketOnly = true) :
    (evalRequest req).allowed = false := by
  cases hAuth : req.authKind <;>
    simp [evalRequest, hChan, hAuth, hSockOnly, Decision.deny]

/-- Main Property 5 safety theorem on current origin/main (post-R-05):
    `socket_only` endpoints can ONLY succeed via the local UNIX socket! -/
theorem prop5_socket_only_holds (req : AuthRequest)
    (hSockOnly : req.endpoint.isSocketOnly = true)
    (hAllow : (evalRequest req).allowed = true) :
    req.channel = .socket := by
  cases hChan : req.channel
  · rfl
  · have hDen := prop5_socket_only_node_forward_denied req hChan hSockOnly
    rw [hAllow] at hDen
    contradiction
  · have hDen := prop5_socket_only_public_http_denied req hChan hSockOnly
    rw [hAllow] at hDen
    contradiction

/-- Historical counterexample on pre-R-05 (`7bcdba6`): before PR #83, `nodeapi`
    mounted `srv.Handler(true)` without `!forwardedFromNode(r)`, bypassing `socket_only`. -/
theorem prop5_pre_r05_node_forward_bypass_counterexample :
    (evalRequestPreR05 ⟨.nodeForward, .bearerToken, .viewer, false, .valid, .authReset, .other⟩).allowed = true ∧
    (evalRequestPreR05 ⟨.nodeForward, .bearerToken, .viewer, false, .valid, .backupRestore, .other⟩).allowed = true ∧
    (evalRequestPreR05 ⟨.nodeForward, .bearerToken, .viewer, false, .valid, .clusterToken, .other⟩).allowed = true := by
  decide

/-! ## Property 6: `MustChange` Sessions Restricted to Password-Change Flow -/

theorem prop6_must_change_restricted (req : AuthRequest)
    (hChan : req.channel = .publicHTTP)
    (hAuth : req.authKind = .sessionCookie)
    (hMust : req.sessState = .mustChange)
    (hAllow : (evalRequest req).allowed = true) :
    req.endpoint.isPreMuxPublic = true ∨
    req.endpoint.isLogin = true ∨
    req.endpoint = .authSession ∨
    req.endpoint = .authLogout ∨
    req.endpoint = .authPassword := by
  cases hEp : req.endpoint <;>
    simp_all [evalRequest, evalGuarded, Endpoint.isPreMuxPublic, Endpoint.isLogin,
      Endpoint.isPublicExt, Endpoint.allowedBeforePasswordChange, Decision.deny]

theorem prop6_must_change_other_endpoints_denied (req : AuthRequest)
    (hChan : req.channel = .publicHTTP)
    (hAuth : req.authKind = .sessionCookie)
    (hMust : req.sessState = .mustChange)
    (hNotPre : req.endpoint.isPreMuxPublic = false)
    (hNotLogin : req.endpoint.isLogin = false)
    (hNotAllowed : req.endpoint.allowedBeforePasswordChange = false) :
    evalRequest req = Decision.deny .passwordChangeRequired := by
  cases hPub : req.endpoint.isPublicExt <;>
    simp [evalRequest, hChan, hAuth, hMust, hNotPre, hNotLogin, hPub, hNotAllowed]

/-! ## Property 7: Session Logout & Token Revocation Across Nodes & Restarts -/

theorem prop7_logout_revokes_across_cluster_and_restart
    (st : ClusterState) (targetID ttl : Nat) (tok : SessionToken) (tn : NodeState)
    (hProto : st.protocol ≥ 24)
    (hFind : st.nodes.find? (fun n => n.nodeID == targetID) = some tn)
    (hValid : validateSession tn tok = true) :
    let st' := logoutOnNode st targetID tok ttl
    ∀ m ∈ st'.nodes,
      validateSession m tok = false ∧
      validateSession (restartNode m) tok = false := by
  intro st' m hm
  have hClk : tn.clock < tok.expiry := by
    simp [validateSession] at hValid
    exact hValid.1.2
  have hUntilGt : max tok.expiry (tn.clock + ttl) > tn.clock := by
    omega
  simp [st', logoutOnNode, hFind, hValid, hProto] at hm
  rcases hm with ⟨orig, _, rfl⟩
  simp [validateSession, restartNode, isRevokedIn, hUntilGt]

/-- Proves that `revokedUntil := max tok.expiry (tn.clock + ttl)` keeps a renewed copy
    `tokRenewed` (whose expiry is at most `tn.clock + ttl` under constant TTL) revoked
    across time advancement and Raft/authfile pruning (`advanceAndPruneNode`) on EVERY node
    and after restart, for as long as `newClock < tokRenewed.expiry`. -/
theorem prop7_logout_revokes_renewed_copy_after_prune
    (st : ClusterState) (targetID ttl newClock : Nat)
    (tok tokRenewed : SessionToken) (tn : NodeState)
    (hProto : st.protocol ≥ 24)
    (hSameID : tokRenewed.id = tok.id)
    (hRenewBound : tokRenewed.expiry ≤ tn.clock + ttl)
    (hBeforeExp : newClock < tokRenewed.expiry)
    (hFind : st.nodes.find? (fun n => n.nodeID == targetID) = some tn)
    (hValid : validateSession tn tok = true) :
    let st' := logoutOnNode st targetID tok ttl
    ∀ m ∈ st'.nodes,
      let m' := advanceAndPruneNode m newClock
      validateSession m' tokRenewed = false ∧
      validateSession (restartNode m') tokRenewed = false := by
  intro st' m hm m'
  have hClk : tn.clock < tok.expiry := by
    simp [validateSession] at hValid
    exact hValid.1.2
  have hUntilGt : max tok.expiry (tn.clock + ttl) > tn.clock := by
    omega
  have hNotPrunedDisk : newClock < max tok.expiry (tn.clock + ttl) + revokedGrace := by
    omega
  simp [st', logoutOnNode, hFind, hValid, hProto] at hm
  rcases hm with ⟨orig, _, rfl⟩
  simp [m', advanceAndPruneNode, validateSession, restartNode, isRevokedIn, hUntilGt, hSameID, hNotPrunedDisk]

/-- Counterexample (Bug Candidate #4): when `protocol < 24` (`ErrClusterUpgrading`),
    `Logout` only records the revocation in memory on the receiving node, so another
    cluster node (or the same node after restart) still accepts the session! -/
theorem prop7_logout_during_upgrade_counterexample :
    ∃ (st : ClusterState) (tok : SessionToken) (n1 n2 : NodeState),
      st.protocol < 24 ∧
      n1 ∈ st.nodes ∧ n2 ∈ st.nodes ∧ n1.nodeID ≠ n2.nodeID ∧
      let st' := logoutOnNode st n1.nodeID tok 100
      ∃ m2 ∈ st'.nodes, m2.nodeID = n2.nodeID ∧ validateSession m2 tok = true := by
  let n1 : NodeState := ⟨1, 1, 10, [], []⟩
  let n2 : NodeState := ⟨2, 1, 10, [], []⟩
  let st : ClusterState := ⟨23, [n1, n2]⟩
  let tok : SessionToken := ⟨42, "admin", 1, 200⟩
  refine ⟨st, tok, n1, n2, by decide, by decide, by decide, by decide, ?_⟩
  refine ⟨n2, ?_, rfl, by decide⟩
  decide

/-- Counterexample (Bug Candidate #5 / R-02): deleting an external user does not
    revoke their personal API tokens in `token.Service` on `origin/main`. -/
theorem prop7_deleted_external_user_token_counterexample :
    ∃ (sys : AuthSystem) (tid : Nat),
      let sys' := deleteExternalUser sys "sso:alice"
      !("sso:alice" ∈ sys'.activeUsers) ∧
      authenticateToken sys' tid 10 = true := by
  let sys : AuthSystem := ⟨["sso:alice"], [⟨1, "sso:alice", false, false, 0⟩]⟩
  refine ⟨sys, 1, by decide, by decide⟩

/-! ## License Feature & Plan Properties (`pkg/ext/license`) -/

theorem license_plan_monotonic_lite_team (f : Feature)
    (hOpt : optInModule f = none) :
    ({ plan := .lite, modules := [], maxNodes := 0, expires := 10, graceDays := 0 } : License).has f = true →
    ({ plan := .team, modules := [], maxNodes := 0, expires := 10, graceDays := 0 } : License).has f = true := by
  cases f <;> simp [License.has, optInModule, currentPlan, planFeatures] at hOpt ⊢

theorem license_plan_monotonic_team_business (f : Feature)
    (hOpt : optInModule f = none) :
    ({ plan := .team, modules := [], maxNodes := 0, expires := 10, graceDays := 0 } : License).has f = true →
    ({ plan := .business, modules := [], maxNodes := 0, expires := 10, graceDays := 0 } : License).has f = true := by
  cases f <;> simp [License.has, optInModule, currentPlan, planFeatures] at hOpt ⊢

theorem license_optin_not_granted_by_wildcard_or_trial (p : Plan) (start : Nat) :
    ({ plan := p, modules := ["*"], maxNodes := 0, expires := 10, graceDays := 0 } : License).has .diff = false ∧
    ({ plan := p, modules := ["*"], maxNodes := 0, expires := 10, graceDays := 0 } : License).has .masking = false ∧
    (trialLicense start).has .diff = false ∧
    (trialLicense start).has .masking = false := by
  simp [License.has, optInModule, trialLicense]

theorem license_usable_iff_valid_or_grace (l : License) (now nodes : Nat) :
    (l.state now nodes).usable = true ↔
      (l.state now nodes = .valid ∨ l.state now nodes = .grace) := by
  cases h : l.state now nodes <;> simp [LicenseState.usable]

theorem license_over_nodes_never_usable (l : License) (now nodes : Nat)
    (hMax : l.maxNodes > 0) (hOver : nodes > l.maxNodes) :
    (l.state now nodes).usable = false := by
  simp [License.state, hMax, hOver, LicenseState.usable]

end DbBranchAuthz.Theorems
