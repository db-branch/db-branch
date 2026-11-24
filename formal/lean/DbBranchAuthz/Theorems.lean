import DbBranchAuthz.Types
import DbBranchAuthz.Model
import DbBranchAuthz.Session
import DbBranchAuthz.License

namespace DbBranchAuthz.Theorems

open DbBranchAuthz
open DbBranchAuthz.Session
open DbBranchAuthz.License

/-! ## Property 1: Default Deny & OSS Design Specification -/

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

/-- Design Specification (#6): in OSS mode without an external `Ext.Authorizer`
    module, `AllowAuthenticated` treats every valid non-anonymous, non-`scope:branches`
    authenticated principal (including `viewer` or empty roles) as equivalent to
    an admin for non-socket-only, non-branch-ownership endpoints such as
    `cluster.remove`, `setting.write`, and `base.delete`. Role-based RBAC is the
    responsibility of the paid `auth` module (`Ext.Authorizer`). -/
theorem design_oss_without_ext_authorizer_authenticated_has_admin_access
    (r : Role) (isTok : Bool) (ep : Endpoint)
    (hRole : r = .viewer ∨ r = .emptyRole)
    (hEp : ep = .clusterRemove ∨ ep = .settingWrite ∨ ep = .baseDelete) :
    (evalRequest ⟨.publicHTTP, .bearerToken, r, isTok, .valid, ep, .other⟩).allowed = true := by
  rcases hRole with rfl | rfl <;> rcases hEp with rfl | rfl | rfl <;> cases isTok <;> decide

/-! ## Property 2: No Privilege Escalation / `branches` Scope Whitelist & Personal Token Roles -/

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
        tokenScopedAuthorize, branchesAllowedList, Decision.deny]
  · cases hAuth : req.authKind <;> cases hSess : req.sessState <;>
      simp [evalRequest, evalGuarded, hChan, hAuth, hSess, hEp, hRole, Endpoint.isPreMuxPublic,
        Endpoint.isLogin, Endpoint.isPublicExt, Endpoint.anonymousAllowed,
        Endpoint.allowedBeforePasswordChange, Endpoint.isGet, Endpoint.action,
        tokenScopedAuthorize, branchesAllowedList, Decision.deny]

theorem prop2_branches_token_secret_reads_denied :
    tokenScopedAuthorize .scopeBranches Endpoint.settingRead.action = false ∧
    tokenScopedAuthorize .scopeBranches Endpoint.tokenRead.action = false ∧
    tokenScopedAuthorize .scopeBranches Endpoint.baseRefreshRead.action = false ∧
    tokenScopedAuthorize .scopeBranches Endpoint.routeUserRead.action = false ∧
    tokenScopedAuthorize .scopeBranches Endpoint.backupRead.action = false := by
  decide

/-- R-02 fixed Bug #2a: `token.Scoped` enforces the explicit whitelist
    (`branchesAllowedList`), rejecting `cluster.read`, `license.read`,
    `module.read`, `update.read`, `ext.<mod>.read`, and all secret reads
    with `403 forbidden` across `evalRequest`. -/
theorem prop2_branches_token_nonbranch_reads_denied :
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .clusterRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .licenseRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .moduleRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .updateRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .extPrivateRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .settingRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .tokenRead, .other⟩ = Decision.deny .forbidden ∧
    evalRequest ⟨.publicHTTP, .bearerToken, .scopeBranches, true, .valid, .backupRead, .other⟩ = Decision.deny .forbidden := by
  decide

private theorem evalGuarded_scopeBranches_denied (localFlag isLoc isBuiltIn validID hasSess : Bool) (isTok : Bool)
    (ep : Endpoint) (rel : OwnerRel)
    (hNotAllowed : branchesAllowedList.contains ep.action = false) :
    (evalGuarded localFlag isLoc isBuiltIn validID hasSess .scopeBranches isTok ep rel).allowed = false := by
  have hScoped : tokenScopedAuthorize .scopeBranches ep.action = false := by
    simpa [tokenScopedAuthorize] using hNotAllowed
  unfold evalGuarded
  split
  · rfl
  · simp [hScoped, Decision.deny]

/-- Universal R-02 property: ANY non-pre-mux, non-login, non-public-ext endpoint whose
    action is outside `branchesAllowedList` is rejected on all remote channels
    (`publicHTTP` and `nodeForward`) for `scope:branches`. -/
theorem prop2_branches_token_denies_all_unlisted_endpoints (req : AuthRequest)
    (hRole : req.role = .scopeBranches)
    (hNotSocket : req.channel ≠ .socket)
    (hNotPre : req.endpoint.isPreMuxPublic = false)
    (hNotLogin : req.endpoint.isLogin = false)
    (hNotPubExt : req.endpoint.isPublicExt = false)
    (hNotAllowed : branchesAllowedList.contains req.endpoint.action = false) :
    (evalRequest req).allowed = false := by
  cases hChan : req.channel
  · contradiction
  · cases hAuth : req.authKind <;>
      cases hSock : req.endpoint.isSocketOnly <;>
      simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hSock, hRole, Decision.deny,
        evalGuarded_scopeBranches_denied _ _ _ _ _ _ _ _ hNotAllowed]
    all_goals simp [evalGuarded, Decision.deny]
  · cases hAuth : req.authKind
    · cases hAnon : req.endpoint.anonymousAllowed
      · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hAnon, Decision.deny]
      · have hFalse : branchesAllowedList.contains req.endpoint.action = true := by
          cases hEp : req.endpoint <;> (rw [hEp] at hAnon; simp [Endpoint.anonymousAllowed] at hAnon) <;> decide
        rw [hFalse] at hNotAllowed
        contradiction
    · cases hSess : req.sessState <;>
        cases hGet : req.endpoint.isGet <;>
        cases hMust : req.endpoint.allowedBeforePasswordChange <;>
        cases hMask : (req.endpoint == .baseMask) <;>
        simp [evalRequest, hChan, hAuth, hSess, hNotPre, hNotLogin, hNotPubExt, hGet, hMust, hMask,
          hRole, Decision.deny, evalGuarded_scopeBranches_denied _ _ _ _ _ _ _ _ hNotAllowed]
    · cases hMask : (req.endpoint == .baseMask) <;>
        simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, hNotPubExt, hRole, hMask, Decision.deny,
          evalGuarded_scopeBranches_denied _ _ _ _ _ _ _ _ hNotAllowed]
    · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, Decision.deny]
    · simp [evalRequest, hChan, hAuth, hNotPre, hNotLogin, Decision.deny]

/-- Model of `viewerReadsOnly` (an `Ext.Authorizer` checking role list for `"viewer"`). -/
def viewerReadsOnlyRoles (roles : List String) (action : String) : Bool :=
  if roles.contains "viewer" then
    isDotReadAction action || action == "auth.login" || action == "auth.session" ||
    action == "auth.logout" || action == "auth.password" || action == "auth.reset" ||
    action == "capability.read"
  else
    true

private theorem personalRoles_foldl_preserves_exclusion (roles init : List String) (bad : String)
    (hInit : bad ∉ init)
    (hBad : bad = "" ∨ bad = "admin" ∨ isScopeRole bad = true) :
    bad ∉ roles.foldl (fun acc r =>
      if r == "" || r == "admin" || isScopeRole r || acc.contains r then
        acc
      else
        acc ++ [r]) init := by
  induction roles generalizing init with
  | nil => exact hInit
  | cons x xs ih =>
    simp only [List.foldl_cons]
    split
    · exact ih init hInit
    · rename_i hCond
      apply ih
      intro hMem
      simp only [List.mem_append, List.mem_singleton] at hMem
      rcases hMem with hOld | hEq
      · exact hInit hOld
      · subst hEq
        simp only [Bool.or_eq_true, beq_iff_eq] at hCond
        rcases hBad with rfl | rfl | hScope
        · exact hCond (Or.inl (Or.inl (Or.inl rfl)))
        · exact hCond (Or.inl (Or.inl (Or.inr rfl)))
        · exact hCond (Or.inl (Or.inr hScope))

private theorem personalRoles_foldl_keeps_existing (roles init : List String) (x : String)
    (hIn : x ∈ init) :
    x ∈ roles.foldl (fun acc r =>
      if r == "" || r == "admin" || isScopeRole r || acc.contains r then
        acc
      else
        acc ++ [r]) init := by
  induction roles generalizing init with
  | nil => exact hIn
  | cons y ys ih =>
    simp only [List.foldl_cons]
    split
    · exact ih init hIn
    · apply ih
      exact List.mem_append.mpr (Or.inl hIn)

private theorem personalRoles_foldl_keeps_valid (roles init : List String) (r : String)
    (hMem : r ∈ roles)
    (hNonEmpty : r ≠ "")
    (hNotAdmin : r ≠ "admin")
    (hNotScope : isScopeRole r = false) :
    r ∈ roles.foldl (fun acc x =>
      if x == "" || x == "admin" || isScopeRole x || acc.contains x then
        acc
      else
        acc ++ [x]) init := by
  induction roles generalizing init with
  | nil => cases hMem
  | cons y ys ih =>
    simp only [List.foldl_cons]
    rcases List.mem_cons.mp hMem with rfl | hTail
    · split
      · rename_i hCond
        simp [hNonEmpty, hNotAdmin, hNotScope] at hCond
        exact personalRoles_foldl_keeps_existing ys init r hCond
      · exact personalRoles_foldl_keeps_existing ys (init ++ [r]) r (by simp)
    · split
      · exact ih init hTail
      · exact ih (init ++ [y]) hTail

/-- R-02 fixed Bug #2b: personal tokens strip `"admin"` and `"scope:*"` roles
    (never elevating a user or carrying `"scope:full"`), while preserving all
    non-admin roles such as `"viewer"` so `Ext.Authorizer` restricts them. -/
theorem prop2_personal_token_strips_admin_and_preserves_owner_roles (ownerRoles : List String) :
    "admin" ∉ personalTokenRoles "branches" ownerRoles ∧
    "scope:full" ∉ personalTokenRoles "branches" ownerRoles ∧
    (∀ r ∈ ownerRoles, r ≠ "" → r ≠ "admin" → isScopeRole r = false →
      r ∈ personalTokenRoles "branches" ownerRoles) := by
  refine ⟨?_, ?_, ?_⟩
  · simp only [personalTokenRoles, scopeRoleName, personalRoles, List.mem_cons]
    rintro (hEq | hMem)
    · exact absurd hEq (by decide)
    · exact personalRoles_foldl_preserves_exclusion ownerRoles [] "admin" (by simp) (Or.inr (Or.inl rfl)) hMem
  · simp only [personalTokenRoles, scopeRoleName, personalRoles, List.mem_cons]
    rintro (hEq | hMem)
    · exact absurd hEq (by decide)
    · exact personalRoles_foldl_preserves_exclusion ownerRoles [] "scope:full" (by simp) (Or.inr (Or.inr (by decide))) hMem
  · intro r hMem hNE hNA hNS
    simp only [personalTokenRoles, scopeRoleName, personalRoles, List.mem_cons]
    exact Or.inr (personalRoles_foldl_keeps_valid ownerRoles [] r hMem hNE hNA hNS)

theorem prop2_viewer_personal_token_cannot_delete_branch :
    viewerReadsOnlyRoles (personalTokenRoles "branches" ["viewer"]) Endpoint.branchDelete.action = false ∧
    viewerReadsOnlyRoles (personalTokenRoles "branches" ["viewer"]) Endpoint.branchGet.action = true := by
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

/-- Post-F-09: `ownsBranch` matches immutable `p.ID` only, so for every non-admin,
    non-anonymous principal, `canReadBranch` holds iff the branch is owned by ID,
    shared via `SharesBranches`, or open (no `hMigrated` exclusion needed). -/
theorem prop4_branch_read_visibility (r : Role) (isTok : Bool) (rel : OwnerRel)
    (hNonAdmin : isAdmin r = false)
    (hNonAnon : r ≠ .anonymous) :
    canReadBranch r isTok rel = true ↔
      (rel = .ownByID ∨ rel = .shared ∨ rel = .openBranch) := by
  cases rel <;> simp [canReadBranch, canManageBranch, ownsBranch, hNonAdmin, hNonAnon]

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

/-- F-09 fixed Bug #3: legacy unmigrated branches (`OwnerName == ""`) never match
    by display name (`ownsBranch` is immutable ID only), so non-admins cannot
    hijack them even when their display name collides with `b.Owner`. -/
theorem prop4_legacy_unmigrated_name_collision_denied (r : Role) (isTok : Bool)
    (hNonAdmin : isAdmin r = false) :
    canManageBranch r isTok .ownByLegacyName = false ∧
    canReadBranch r isTok .ownByLegacyName = false := by
  simp [canReadBranch, canManageBranch, ownsBranch, hNonAdmin]

theorem prop4_legacy_unmigrated_http_delete_denied :
    evalRequest ⟨.publicHTTP, .bearerToken, .viewer, false, .valid, .branchDelete, .ownByLegacyName⟩ =
      Decision.deny .forbidden := by
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

/-- On post-R-05 `main`, `nodeForward` ALWAYS rejects `socket_only` operations. -/
theorem prop5_socket_only_node_forward_denied (req : AuthRequest)
    (hChan : req.channel = .nodeForward)
    (hSockOnly : req.endpoint.isSocketOnly = true) :
    (evalRequest req).allowed = false := by
  cases hAuth : req.authKind <;>
    simp [evalRequest, hChan, hAuth, hSockOnly, Decision.deny]

/-- Main Property 5 safety theorem: `socket_only` endpoints can ONLY succeed via the local UNIX socket. -/
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
    let res := logoutOnNode st targetID tok ttl
    res.2 = true ∧
    ∀ m ∈ res.1.nodes,
      validateSession m tok = false ∧
      validateSession (restartNode m) tok = false := by
  intro res
  have hClk : tn.clock < tok.expiry := by
    simp [validateSession] at hValid
    exact hValid.1.2
  have hUntilGt : max tok.expiry (tn.clock + ttl) > tn.clock := by
    omega
  constructor
  · simp [res, logoutOnNode, hFind, hValid, hProto]
  · intro m hm
    simp [res, logoutOnNode, hFind, hValid, hProto] at hm
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
    let st' := (logoutOnNode st targetID tok ttl).1
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

/-- F-09 fixed Bug #4: during a mixed-cluster upgrade (`protocol < 24`), `Logout`
    never silently returns 204 (`httpLogoutOK = false`, i.e. `503 cluster_upgrading`),
    revokes locally on `targetID`, and queues the revocation in `pendingRevoked`;
    once the cluster reaches `protocol >= 24`, `flushPendingRevocations` propagates
    it to all nodes and restarts. -/
theorem prop7_logout_during_upgrade_reports_503_and_flushes
    (st : ClusterState) (targetID ttl newProto : Nat) (tok : SessionToken) (tn : NodeState)
    (hOldProto : st.protocol < 24)
    (hNewProto : newProto ≥ 24)
    (hFind : st.nodes.find? (fun n => n.nodeID == targetID) = some tn)
    (hNodup : ∀ n ∈ st.nodes, n.nodeID = targetID → n = tn)
    (hValid : validateSession tn tok = true) :
    let res := logoutOnNode st targetID tok ttl
    let stUpgraded := upgradeCluster res.1 newProto
    let stFlushed := flushPendingRevocations stUpgraded targetID
    res.2 = false ∧
    (∀ m ∈ res.1.nodes, m.nodeID = targetID → validateSession m tok = false) ∧
    (∀ m ∈ stFlushed.nodes,
      validateSession m tok = false ∧
      validateSession (restartNode m) tok = false) := by
  intro res stUpgraded stFlushed
  have hNotGe : ¬ (st.protocol ≥ 24) := by omega
  have hClk : tn.clock < tok.expiry := by
    simp [validateSession] at hValid
    exact hValid.1.2
  have hUntilGt : tn.clock < max tok.expiry (tn.clock + ttl) := by
    omega
  have hRes1 : res.1 = { st with
      nodes := st.nodes.map fun n =>
        if n.nodeID == targetID then
          { n with
            memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.memRevoked.filter (fun (_, exp) => tn.clock < exp)
            pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) }
        else n } := by
    simp [res, logoutOnNode, hFind, hValid, hNotGe]
  have hTargetUpdated : stUpgraded.nodes.find? (fun n => n.nodeID == targetID) = some
      { tn with
        memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: tn.memRevoked.filter (fun (_, exp) => tn.clock < exp)
        pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: tn.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) } := by
    have hFindMap : ∀ (xs : List NodeState),
        xs.find? (fun n => n.nodeID == targetID) = some tn →
        (∀ n ∈ xs, n.nodeID = targetID → n = tn) →
        (xs.map fun n =>
          if n.nodeID == targetID then
            { n with
              memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.memRevoked.filter (fun (_, exp) => tn.clock < exp)
              pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) }
          else n).find? (fun n => n.nodeID == targetID) = some
          { tn with
            memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: tn.memRevoked.filter (fun (_, exp) => tn.clock < exp)
            pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: tn.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) } := by
      intro xs hf huniq
      induction xs with
      | nil => simp at hf
      | cons x xs ih =>
        rw [List.find?_cons] at hf
        rw [List.map_cons, List.find?_cons]
        cases hx : (x.nodeID == targetID)
        · simp only [hx, Bool.false_eq_true, ↓reduceIte] at hf ⊢
          exact ih hf (fun n hn => huniq n (List.mem_cons_of_mem x hn))
        · simp only [hx, ↓reduceIte] at hf ⊢
          injection hf with hEq
          have hxtn : x = tn := huniq x (List.mem_cons_self ..) (beq_iff_eq.mp hx)
          rw [← hEq, hxtn]
    simp only [stUpgraded, upgradeCluster, hRes1]
    exact hFindMap st.nodes hFind hNodup
  refine ⟨?_, ?_, ?_⟩
  · simp [res, logoutOnNode, hFind, hValid, hNotGe]
  · intro m hm hid
    rw [hRes1] at hm
    simp only [List.mem_map] at hm
    rcases hm with ⟨orig, _, rfl⟩
    cases hEq : (orig.nodeID == targetID)
    · simp only [hEq] at hid
      exact absurd hid (by simpa [beq_iff_eq] using hEq)
    · simp [validateSession, isRevokedIn]
  · intro m hm
    have hFlushNotLt : ¬ (stUpgraded.protocol < 24) := by
      simp [stUpgraded, upgradeCluster]
      omega
    simp only [stFlushed, flushPendingRevocations, hFlushNotLt, ↓reduceIte, hTargetUpdated, List.mem_map] at hm
    rcases hm with ⟨orig, _, rfl⟩
    simp [validateSession, restartNode, isRevokedIn, hUntilGt]

/-- F-09 `PendingSession` retry & time advancement: after a 503 during upgrade
    (`protocol < 24`), even after time advances (`newClock < tok.expiry`), once
    upgraded to `protocol >= 24`, retrying `POST /v1/auth/logout` on `targetID`
    returns `httpLogoutOK = true` (204) and replicates the revocation cluster-wide
    and across restarts. -/
theorem prop7_logout_during_upgrade_retry_replicates
    (st : ClusterState) (targetID ttl newProto newClock : Nat) (tok : SessionToken) (tn : NodeState)
    (hOldProto : st.protocol < 24)
    (hNewProto : newProto ≥ 24)
    (hBeforeExp : newClock < tok.expiry)
    (hFind : st.nodes.find? (fun n => n.nodeID == targetID) = some tn)
    (hNodup : ∀ n ∈ st.nodes, n.nodeID = targetID → n = tn)
    (hValid : validateSession tn tok = true) :
    let res := logoutOnNode st targetID tok ttl
    let stAdvanced : ClusterState := { protocol := newProto, nodes := res.1.nodes.map (fun n => advanceAndPruneNode n newClock) }
    let resRetry := logoutOnNode stAdvanced targetID tok ttl
    res.2 = false ∧
    resRetry.2 = true ∧
    ∀ m ∈ resRetry.1.nodes,
      validateSession m tok = false ∧
      validateSession (restartNode m) tok = false := by
  intro res stAdvanced resRetry
  have hNotGe : ¬ (st.protocol ≥ 24) := by omega
  have hGen : tok.gen = tn.currentGen := by
    simp [validateSession] at hValid
    exact hValid.1.1
  have hNotDisk : isRevokedIn tok.id tn.diskRevoked = false := by
    simp [validateSession] at hValid
    exact hValid.2.2
  have hUntilGt : newClock < max tok.expiry (tn.clock + ttl) := by
    omega
  have hRes1 : res.1 = { st with
      nodes := st.nodes.map fun n =>
        if n.nodeID == targetID then
          { n with
            memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.memRevoked.filter (fun (_, exp) => tn.clock < exp)
            pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) }
        else n } := by
    simp [res, logoutOnNode, hFind, hValid, hNotGe]
  let tnAfter : NodeState := advanceAndPruneNode
    { tn with
      memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: tn.memRevoked.filter (fun (_, exp) => tn.clock < exp)
      pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: tn.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) } newClock
  have hFindAdv : stAdvanced.nodes.find? (fun n => n.nodeID == targetID) = some tnAfter := by
    have hFindMap : ∀ (xs : List NodeState),
        xs.find? (fun n => n.nodeID == targetID) = some tn →
        (∀ n ∈ xs, n.nodeID = targetID → n = tn) →
        (xs.map fun n => advanceAndPruneNode (
          if n.nodeID == targetID then
            { n with
              memRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.memRevoked.filter (fun (_, exp) => tn.clock < exp)
              pendingRevoked := (tok.id, max tok.expiry (tn.clock + ttl)) :: n.pendingRevoked.filter (fun (_, exp) => tn.clock < exp) }
          else n) newClock).find? (fun n => n.nodeID == targetID) = some tnAfter := by
      intro xs hf huniq
      induction xs with
      | nil => simp at hf
      | cons x xs ih =>
        rw [List.find?_cons] at hf
        rw [List.map_cons, List.find?_cons]
        cases hx : (x.nodeID == targetID)
        · simp only [hx, advanceAndPruneNode, Bool.false_eq_true, ↓reduceIte] at hf ⊢
          exact ih hf (fun n hn => huniq n (List.mem_cons_of_mem x hn))
        · simp only [tnAfter, hx, advanceAndPruneNode, ↓reduceIte] at hf ⊢
          injection hf with hEq
          have hxtn : x = tn := huniq x (List.mem_cons_self ..) (beq_iff_eq.mp hx)
          rw [← hEq, hxtn]
    simp only [stAdvanced, hRes1, List.map_map]
    exact hFindMap st.nodes hFind hNodup
  have hPend : validatePendingSession tnAfter tok = true := by
    simp only [isRevokedIn, Bool.eq_false_iff, ne_eq, List.any_eq_true, not_exists, not_and, beq_iff_eq] at hNotDisk
    simp [tnAfter, advanceAndPruneNode, validatePendingSession, hGen, hBeforeExp, isRevokedIn, hUntilGt]
    intro a b hab _
    exact hNotDisk (a, b) hab
  have hRetryClk : tnAfter.clock = newClock := rfl
  have hMaxGt : newClock < max tok.expiry (newClock + ttl) := by omega
  have hAdvProto : stAdvanced.protocol ≥ 24 := by simp [stAdvanced]; omega
  refine ⟨?_, ?_, ?_⟩
  · simp [res, logoutOnNode, hFind, hValid, hNotGe]
  · simp [resRetry, logoutOnNode, hFindAdv, hPend, hAdvProto]
  · intro m hm
    simp only [resRetry, logoutOnNode, hFindAdv, hPend, Bool.or_true, ↓reduceIte, hAdvProto] at hm
    rcases List.mem_map.mp hm with ⟨orig, _, rfl⟩
    simp [validateSession, restartNode, isRevokedIn, hRetryClk, hMaxGt]

/-- R-02 fixed Bug #5: deleting an external user immediately revokes all of
    their personal API tokens so `authenticateToken` rejects them. -/
theorem prop7_deleted_external_user_revokes_personal_tokens
    (sys : AuthSystem) (user : String) (tid now : Nat)
    (hUser : user ≠ "")
    (hOwned : ∀ t ∈ sys.tokens, t.id = tid → t.owner = user) :
    authenticateToken (deleteExternalUser sys user) tid now = false := by
  have hNone : (revokeTokensByOwner sys.tokens user).find? (fun t => t.id == tid) = none := by
    simp only [revokeTokensByOwner, beq_iff_eq, hUser, ↓reduceIte]
    rw [List.find?_eq_none]
    intro t ht
    rw [List.mem_filter] at ht
    intro hId
    have hEq := hOwned t ht.1 (beq_iff_eq.mp hId)
    simp [hEq] at ht
  simp [authenticateToken, deleteExternalUser, hNone]

/-- R-02 safety: `RevokeByOwner` never deletes service tokens (`owner == ""`)
    or another user's personal tokens. -/
theorem prop7_deleted_external_user_keeps_service_and_other_tokens
    (sys : AuthSystem) (user : String) (t : APIToken)
    (hIn : t ∈ sys.tokens)
    (hOther : t.owner ≠ user) :
    t ∈ (deleteExternalUser sys user).tokens := by
  simp only [deleteExternalUser, revokeTokensByOwner]
  split
  · exact hIn
  · rw [List.mem_filter]
    exact ⟨hIn, by simp [hOther]⟩

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
