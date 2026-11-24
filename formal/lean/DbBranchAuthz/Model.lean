import DbBranchAuthz.Types

namespace DbBranchAuthz

def Endpoint.isGet : Endpoint → Bool
  | .healthz | .readyz | .tlsPem | .authMethods | .authSession
  | .branchList | .branchGet | .branchDSN | .branchCheckpointRead
  | .baseList | .baseGet | .basePublic | .baseRefreshRead | .uploadRoom
  | .routeUserRead | .tokenRead | .accountPreferenceRead | .accountTokenRead
  | .settingRead | .licenseRead | .licenseRenewalRead | .moduleRead | .updateRead
  | .runtimeRead | .runtimeAvailable | .operationList | .operationGet
  | .eventRead | .capabilityRead | .clusterRead | .nodeMetrics | .nodeStorage | .metricsRead
  | .backupRead | .backupDownload
  | .extPrivateRead | .extPublicRead => true
  | _ => false

def Endpoint.isPreMuxPublic : Endpoint → Bool
  | .healthz | .readyz | .tlsPem | .authMethods => true
  | _ => false

def Endpoint.isLogin : Endpoint → Bool
  | .authLogin => true
  | _ => false

def Endpoint.isPublicExt : Endpoint → Bool
  | .extPublicRead | .extPublicWrite => true
  | _ => false

def Endpoint.isSocketOnly : Endpoint → Bool
  | .authReset | .clusterToken | .backupRestore => true
  | _ => false

/-- Action name as registered in server.go routes() and ext.go (origin/main). -/
def Endpoint.action : Endpoint → String
  | .healthz | .readyz | .tlsPem | .authMethods => ""
  | .authLogin => "auth.login"
  | .authSession => "auth.session"
  | .authLogout => "auth.logout"
  | .authPassword => "auth.password"
  | .authReset => "auth.reset"
  | .branchList | .branchGet => "branch.read"
  | .branchDSN => "branch.connect"
  | .branchQuery => "branch.query"
  | .branchCreate => "branch.create"
  | .branchReset => "branch.reset"
  | .branchDelete => "branch.delete"
  | .branchExtend => "branch.extend"
  | .branchPause => "branch.pause"
  | .branchResume => "branch.resume"
  | .branchCheckpointRead => "branch.checkpoint.read"
  | .branchCheckpointCreate => "branch.checkpoint"
  | .branchCheckpointDelete => "branch.checkpoint.delete"
  | .branchRestore => "branch.restore"
  | .branchUpgrade => "branch.upgrade"
  | .branchFork => "branch.fork"
  | .branchMerge => "branch.merge"
  | .branchRecreate => "branch.recreate"
  | .baseList | .baseGet => "base.read"
  | .basePublic => "base.public"
  | .baseCreate => "base.create"
  | .baseDescribe => "base.describe"
  | .baseReplicas => "base.replicas"
  | .basePatchAnonymous | .baseSnapshotPatch => "base.publish"
  | .baseDelete => "base.delete"
  | .baseSnapshot => "base.snapshot"
  | .baseMask => "base.mask"
  | .baseStart => "base.start"
  | .baseStop => "base.stop"
  | .baseSQL => "base.sql"
  | .baseLoadPath | .baseLoadUpload => "base.load"
  | .baseRefreshRead => "base.refresh.read"
  | .baseRefreshWrite | .baseRefreshDelete | .baseRefreshRun => "base.refresh"
  | .uploadCreate | .uploadRoom => "base.upload"
  | .baseSnapshotDelete => "base.snapshot.delete"
  | .routeUserRead => "route-user.read"
  | .routeUserWrite => "route-user.write"
  | .routeUserDelete => "route-user.delete"
  | .tokenRead => "token.read"
  | .tokenWrite | .tokenDelete => "token.write"
  | .accountPreferenceRead => "account.preference.read"
  | .accountPreferenceWrite => "account.preference.write"
  | .accountTokenRead => "account.token.read"
  | .accountTokenCreate | .accountTokenDelete => "account.token.write"
  | .mcpUse => "mcp.use"
  | .settingRead => "setting.read"
  | .settingWrite => "setting.write"
  | .licenseRead | .licenseRenewalRead => "license.read"
  | .licenseVerify => "license.verify"
  | .licenseWrite | .licenseRenew => "license.write"
  | .moduleRead => "module.read"
  | .moduleInstall => "module.install"
  | .moduleRemove => "module.remove"
  | .moduleForget => "module.forget"
  | .updateRead => "update.read"
  | .updateCheck => "update.check"
  | .updateApply | .updateCancel => "update.apply"
  | .runtimeRead | .runtimeAvailable => "runtime.read"
  | .runtimeInstall => "runtime.install"
  | .runtimeRemove => "runtime.remove"
  | .operationList | .operationGet => "operation.read"
  | .eventRead => "event.read"
  | .capabilityRead => "capability.read"
  | .clusterRead | .nodeMetrics | .nodeStorage => "cluster.read"
  | .metricsRead => "metrics.read"
  | .clusterRemove => "cluster.remove"
  | .clusterToken => "cluster.token"
  | .backupRead => "backup.read"
  | .backupCreate => "backup.create"
  | .backupDownload => "backup.download"
  | .backupRestore => "backup.restore"
  | .extPrivateRead | .extPublicRead => "ext.mod.read"
  | .extPrivateWrite | .extPublicWrite => "ext.mod.write"

/-- Resource kind extracted by strings.Cut(action, "."). -/
def Endpoint.resourceKind : Endpoint → String
  | .healthz | .readyz | .tlsPem | .authMethods => ""
  | .authLogin | .authSession | .authLogout | .authPassword | .authReset => "auth"
  | .branchList | .branchGet | .branchDSN | .branchQuery | .branchCreate
  | .branchReset | .branchDelete | .branchExtend | .branchPause | .branchResume
  | .branchCheckpointRead | .branchCheckpointCreate | .branchCheckpointDelete
  | .branchRestore | .branchUpgrade | .branchFork | .branchMerge | .branchRecreate => "branch"
  | .baseList | .baseGet | .basePublic | .baseCreate | .baseDescribe | .baseReplicas
  | .basePatchAnonymous | .baseDelete | .baseSnapshot | .baseMask | .baseStart | .baseStop
  | .baseSQL | .baseLoadPath | .baseLoadUpload | .baseRefreshRead | .baseRefreshWrite
  | .baseRefreshDelete | .baseRefreshRun | .uploadCreate | .uploadRoom
  | .baseSnapshotPatch | .baseSnapshotDelete => "base"
  | .routeUserRead | .routeUserWrite | .routeUserDelete => "route-user"
  | .tokenRead | .tokenWrite | .tokenDelete => "token"
  | .accountPreferenceRead | .accountPreferenceWrite
  | .accountTokenRead | .accountTokenCreate | .accountTokenDelete => "account"
  | .mcpUse => "mcp"
  | .settingRead | .settingWrite => "setting"
  | .licenseRead | .licenseVerify | .licenseWrite | .licenseRenewalRead | .licenseRenew => "license"
  | .moduleRead | .moduleInstall | .moduleRemove | .moduleForget => "module"
  | .updateRead | .updateCheck | .updateApply | .updateCancel => "update"
  | .runtimeRead | .runtimeAvailable | .runtimeInstall | .runtimeRemove => "runtime"
  | .operationList | .operationGet => "operation"
  | .eventRead => "event"
  | .capabilityRead => "capability"
  | .clusterRead | .nodeMetrics | .nodeStorage | .clusterRemove | .clusterToken => "cluster"
  | .metricsRead => "metrics"
  | .backupRead | .backupCreate | .backupDownload | .backupRestore => "backup"
  | .extPrivateRead | .extPrivateWrite | .extPublicRead | .extPublicWrite => "ext"

def Endpoint.hasBranchName : Endpoint → Bool
  | .branchGet | .branchDSN | .branchQuery
  | .branchReset | .branchDelete | .branchExtend | .branchPause | .branchResume
  | .branchCheckpointRead | .branchCheckpointCreate | .branchCheckpointDelete
  | .branchRestore | .branchUpgrade | .branchFork | .branchMerge | .branchRecreate => true
  | _ => false

/-- anonymousAllowed in ownership.go:24-32 and 44-51. -/
def Endpoint.anonymousAllowed : Endpoint → Bool
  | .branchCreate | .branchGet | .branchDSN | .operationGet | .basePublic => true
  | _ => false

/-- allowedBeforePasswordChange in server.go:473-479. -/
def Endpoint.allowedBeforePasswordChange : Endpoint → Bool
  | .authSession | .authLogout | .authPassword => true
  | _ => false

def isAdmin (r : Role) : Bool :=
  r == .localPrincipal || r == .admin || r == .ssoAdmin || r == .scopeFull

/-- secretReads in internal/usecase/token/token.go:212. -/
def isSecretRead (action : String) : Bool :=
  action == "setting.read" || action == "token.read" ||
  action == "base.refresh.read" || action == "route-user.read" ||
  action == "backup.read"

def isBranchAction (action : String) : Bool :=
  action == "branch.read" || action == "branch.create" || action == "branch.extend" ||
  action == "branch.delete" || action == "branch.reset" || action == "branch.update" ||
  action == "branch.pause" || action == "branch.resume" ||
  action == "branch.fork" || action == "branch.recreate" ||
  action == "branch.checkpoint.read" || action == "branch.checkpoint" ||
  action == "branch.checkpoint.delete" || action == "branch.checkpoint_delete" ||
  action == "branch.restore" || action == "branch.upgrade" || action == "branch.merge" ||
  action == "branch.connect" || action == "branch.query"

def isDotReadAction (action : String) : Bool :=
  action == "branch.read" || action == "branch.checkpoint.read" ||
  action == "base.read" || action == "base.refresh.read" ||
  action == "route-user.read" || action == "token.read" ||
  action == "account.token.read" || action == "account.preference.read" ||
  action == "setting.read" || action == "license.read" ||
  action == "module.read" || action == "update.read" ||
  action == "runtime.read" || action == "operation.read" ||
  action == "event.read" || action == "capability.read" ||
  action == "cluster.read" || action == "metrics.read" ||
  action == "backup.read" || action == "ext.mod.read"

/-- Decision 13 canonicalization: maps legacy aliases to new dot-separated action names. -/
def canonicalAction : String → String
  | "branch.checkpoint_delete" => "branch.checkpoint.delete"
  | "base.snapshot_delete"     => "base.snapshot.delete"
  | a                          => a

/-- token.Scoped.Authorize in internal/usecase/token/token.go:214-225 (origin/main). -/
def tokenScopedAuthorize (r : Role) (action : String) : Bool :=
  if r != .scopeBranches then
    true
  else
    isBranchAction action ||
    (isDotReadAction action && !isSecretRead action) ||
    action == "mcp.use" ||
    action == "base.public"

/-- R-02 whitelist version of token.Scoped, normalizing legacy Decision-13 aliases first. -/
def tokenScopedWhitelist (r : Role) (action : String) : Bool :=
  if r != .scopeBranches then
    true
  else
    let act := canonicalAction action
    act == "branch.read" || act == "branch.create" || act == "branch.extend" ||
    act == "branch.delete" || act == "branch.reset" || act == "branch.update" ||
    act == "branch.pause" || act == "branch.resume" ||
    act == "branch.fork" || act == "branch.recreate" ||
    act == "branch.checkpoint.read" || act == "branch.checkpoint" ||
    act == "branch.checkpoint.delete" ||
    act == "branch.restore" || act == "branch.upgrade" || act == "branch.merge" ||
    act == "branch.connect" || act == "branch.query" ||
    act == "base.read" || act == "base.public" ||
    act == "operation.read" || act == "runtime.read" ||
    act == "capability.read" || act == "mcp.use"

/-- ownsBranch in internal/interface/httpapi/ownership.go:110-115. -/
def ownsBranch (isTokenID : Bool) (rel : OwnerRel) : Bool :=
  match rel with
  | .ownByID => true
  | .ownByLegacyName => !isTokenID
  | _ => false

/-- canManageBranch in internal/interface/httpapi/ownership.go:92-103. -/
def canManageBranch (r : Role) (isTokenID : Bool) (rel : OwnerRel) : Bool :=
  if isAdmin r then
    true
  else if r == .anonymous || rel == .noOwner || rel == .unknownBranch || rel == .openBranch then
    false
  else if ownsBranch isTokenID rel then
    true
  else
    rel == .shared

/-- canReadBranch in internal/interface/httpapi/ownership.go:156-158. -/
def canReadBranch (r : Role) (isTokenID : Bool) (rel : OwnerRel) : Bool :=
  rel == .openBranch || canManageBranch r isTokenID rel

/-- ownership.Authorize in internal/interface/httpapi/ownership.go:160-193.
    Returns `(authorized, branchNotFoundInHandler)`. -/
def ownershipAuthorize (r : Role) (isTokenID : Bool) (ep : Endpoint) (rel : OwnerRel) : Bool × Bool :=
  let act := canonicalAction ep.action
  if r == .anonymous then
    match act with
    | "branch.create" | "operation.read" | "base.public" => (true, false)
    | "branch.read" | "branch.connect" =>
      if ep.hasBranchName && rel == .openBranch then (true, false) else (false, false)
    | _ => (false, false)
  else if isAdmin r || ep.resourceKind != "branch" || !ep.hasBranchName || act == "branch.create" then
    (true, ep.hasBranchName && rel == .unknownBranch)
  else if rel == .unknownBranch then
    (true, true)
  else
    match act with
    | "branch.read" | "branch.connect" | "branch.query" =>
      (canReadBranch r isTokenID rel, false)
    | _ =>
      (canManageBranch r isTokenID rel, false)

/-- Full OSS guard + handler authorization (assuming authentication succeeded with validID). -/
def evalGuarded (localFlag : Bool) (isLocalPrincipal : Bool) (isBuiltInAdmin : Bool)
    (validID : Bool) (hasSessionCtx : Bool) (r : Role) (isTokenID : Bool)
    (ep : Endpoint) (rel : OwnerRel) : Decision :=
  if !validID then
    Decision.deny .forbidden
  else if !tokenScopedAuthorize r ep.action then
    Decision.deny .forbidden
  else
    let (ownOK, notFound) := ownershipAuthorize r isTokenID ep rel
    if !ownOK then
      Decision.deny .forbidden
    else if (ep == .authSession || ep == .authPassword) && !hasSessionCtx then
      -- misc.go:358, 388: requires console session cookie (s.current(r))
      Decision.deny .unauthenticated
    else if ep == .authPassword && !isBuiltInAdmin then
      -- misc.go:283-292, 394: checks sess.User == "admin" (not Role!), OSS without PasswordChanger denies
      Decision.deny .managedElsewhere
    else if ep == .accountTokenCreate && (r == .scopeFull || r == .scopeBranches) then
      -- tokens.go:184 rejects any principal carrying a "scope:*" role with 403
      Decision.deny .forbidden
    else if ep == .accountTokenCreate && (isLocalPrincipal || r == .anonymous || isTokenID) then
      -- token.go:65 CreatePersonal rejects "local", "anonymous", and "token:*" with ErrInvalid (400)
      Decision.deny .invalidRequest
    else if ep == .baseLoadPath && !isLocalPrincipal then
      -- uploads.go:87 resolveDump rejects non-upload paths unless principalOf(r).ID == LocalPrincipal.ID
      Decision.deny .invalidRequest
    else if ep.isSocketOnly && !localFlag then
      Decision.deny .socketOnly
    else if notFound then
      Decision.deny .notFound
    else
      Decision.allow

/-- Complete request evaluation modeling Server.Handler(local) + nodeapi.forwardedAPI on origin/main (post-R-05, post-W-22). -/
def evalRequest (req : AuthRequest) : Decision :=
  let ep := req.endpoint
  match req.channel with
  | .socket =>
    -- Handler(true) without forwarding headers: local = true, forwardedIn = false, p = LocalPrincipal ("local", admin)
    -- W-22 (server.go:226, 317): POST /v1/bases/{name}/mask is unrouted unless forwardedFromNode -> 404
    if ep == .baseMask then
      Decision.deny .notFound
    else if ep.isPreMuxPublic || ep.isLogin || ep.isPublicExt then
      Decision.allow
    else
      evalGuarded true true false true false .localPrincipal false ep req.ownerRel
  | .nodeForward =>
    -- nodeapi.forwardedAPI runs OUTSIDE s.API (Handler(true)):
    -- 1. missing/empty/duplicate forwarding headers -> 403 forbidden
    -- 2. socketOnlyRoutes -> 403 socket_only (R-05, even before s.API)
    -- 3. internalMaskTarget routes POST /v1/bases/{name}/mask to guard("base.mask") (W-22)
    match req.authKind with
    | .missingForwardHeaders =>
      Decision.deny .forbidden
    | .malformedForward =>
      if ep.isSocketOnly then
        Decision.deny .socketOnly
      else if ep.isPreMuxPublic || ep.isLogin || ep.isPublicExt then
        Decision.allow
      else
        -- localPrincipal sets p = ext.Principal{} (ID == "") -> AllowAuthenticated denies
        evalGuarded false false false false false .emptyRole false ep req.ownerRel
    | _ =>
      if ep.isSocketOnly then
        Decision.deny .socketOnly
      else if ep.isPreMuxPublic || ep.isLogin || ep.isPublicExt then
        Decision.allow
      else
        let isLoc := req.role == .localPrincipal
        evalGuarded false isLoc false true false req.role req.isTokenID ep req.ownerRel
  | .publicHTTP =>
    -- Handler(false): local = false, wrapped in requireSession (after pre-mux healthz/readyz/tls.pem/authMethods)
    if ep.isPreMuxPublic || ep.isLogin then
      Decision.allow
    else
      match req.authKind with
      | .missingForwardHeaders | .malformedForward =>
        -- Impossible on publicHTTP (forwarding headers stripped)
        Decision.deny .unauthenticated
      | .bearerToken =>
        -- server.go:399: requireSession rejects p.ID == LocalPrincipal.ID ("local") with 401!
        -- ext.go:125: publicExt skips s.guard for valid non-local Bearer callers.
        if req.role == .localPrincipal then
          Decision.deny .unauthenticated
        else if ep == .baseMask then
          -- W-22: POST /v1/bases/{name}/mask is not registered in mux -> 404
          Decision.deny .notFound
        else if ep.isPublicExt then
          Decision.allow
        else
          evalGuarded false false false true false req.role req.isTokenID ep req.ownerRel
      | .none =>
        if ep.isPublicExt then
          Decision.allow
        else if ep.anonymousAllowed then
          evalGuarded false false false true false .anonymous false ep req.ownerRel
        else
          Decision.deny .unauthenticated
      | .sessionCookie =>
        let isBuiltIn := req.role == .admin
        if ep.isPublicExt then
          match req.sessState with
          | .expired | .revoked | .wrongGen =>
            -- server.go:417: if Auth.Session fails on publicExt, falls back to Anonymous
            Decision.allow
          | .localUserSession =>
            Decision.deny .unauthenticated
          | .csrfBad =>
            if ep.isGet then Decision.allow else Decision.deny .csrfFailed
          | .mustChange =>
            if ep.allowedBeforePasswordChange then Decision.allow else Decision.deny .passwordChangeRequired
          | .valid =>
            Decision.allow
        else
          match req.sessState with
          | .expired | .revoked | .wrongGen | .localUserSession =>
            Decision.deny .unauthenticated
          | .csrfBad =>
            if ep.isGet then
              if ep == .baseMask then Decision.deny .notFound
              else evalGuarded false false isBuiltIn true true req.role req.isTokenID ep req.ownerRel
            else
              Decision.deny .csrfFailed
          | .mustChange =>
            if ep.allowedBeforePasswordChange then
              evalGuarded false false isBuiltIn true true req.role req.isTokenID ep req.ownerRel
            else
              Decision.deny .passwordChangeRequired
          | .valid =>
            if ep == .baseMask then
              Decision.deny .notFound
            else
              evalGuarded false false isBuiltIn true true req.role req.isTokenID ep req.ownerRel

/-- Pre-R-05 legacy model (before PR #83) showing the nodeForward socket_only bypass. -/
def evalRequestPreR05 (req : AuthRequest) : Decision :=
  if req.channel == .nodeForward && req.endpoint.isSocketOnly then
    match req.authKind with
    | .missingForwardHeaders => Decision.deny .forbidden
    | .malformedForward => evalGuarded true false false false false .emptyRole false req.endpoint req.ownerRel
    | _ => evalGuarded true (req.role == .localPrincipal) false true false req.role req.isTokenID req.endpoint req.ownerRel
  else
    evalRequest req

/-- legacyActionAliases + legacyActionAuthorizer in internal/interface/httpapi/access.go:122-143 (W-22). -/
def legacyAliases : String → List String
  | "branch.pause"             => ["branch.update"]
  | "branch.resume"            => ["branch.update"]
  | "branch.update"            => ["branch.pause", "branch.resume"]
  | "branch.checkpoint.delete" => ["branch.checkpoint_delete"]
  | "branch.checkpoint_delete" => ["branch.checkpoint.delete"]
  | "base.snapshot.delete"     => ["base.snapshot_delete"]
  | "base.snapshot_delete"     => ["base.snapshot.delete"]
  | _                          => []

def legacyActionAuthorize (baseAllow : String → Bool) (action : String) : Bool :=
  baseAllow action || (legacyAliases action).any baseAllow

/-- Parameterized action-level evaluator (used to prove Decision-13 alias equivalence). -/
def evalActionWithWhitelist (r : Role) (isTok : Bool) (action : String) (rel : OwnerRel) : Bool :=
  let act := canonicalAction action
  let scopedOK := tokenScopedWhitelist r act
  let ownOK :=
    if r == .anonymous then
      act == "branch.create" || act == "operation.read" || act == "base.public" ||
      ((act == "branch.read" || act == "branch.connect") && rel == .openBranch)
    else if isAdmin r || !isBranchAction act || act == "branch.create" then
      true
    else if rel == .unknownBranch then
      true
    else if act == "branch.read" || act == "branch.connect" || act == "branch.query" then
      canReadBranch r isTok rel
    else
      canManageBranch r isTok rel
  scopedOK && ownOK

end DbBranchAuthz
