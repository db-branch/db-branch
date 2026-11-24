import DbBranchAuthz.Types
import DbBranchAuthz.Model
import DbBranchAuthz.License

namespace DbBranchAuthz.JsonExport

open DbBranchAuthz
open DbBranchAuthz.License

def Channel.toString : Channel → String
  | .socket      => "socket"
  | .nodeForward => "node_forward"
  | .publicHTTP  => "public_http"

def AuthKind.toString : AuthKind → String
  | .none                  => "none"
  | .sessionCookie         => "session_cookie"
  | .bearerToken           => "bearer_token"
  | .missingForwardHeaders => "missing_forward_headers"
  | .malformedForward      => "malformed_forward"

def Role.toString : Role → String
  | .localPrincipal => "local_principal"
  | .admin          => "admin"
  | .ssoAdmin       => "sso_admin"
  | .scopeFull      => "scope_full"
  | .scopeBranches  => "scope_branches"
  | .viewer         => "viewer"
  | .anonymous      => "anonymous"
  | .emptyRole      => "empty"

def SessState.toString : SessState → String
  | .valid            => "valid"
  | .mustChange       => "must_change"
  | .csrfBad          => "csrf_bad"
  | .expired          => "expired"
  | .revoked          => "revoked"
  | .wrongGen         => "wrong_gen"
  | .localUserSession => "local_user_session"

def OwnerRel.toString : OwnerRel → String
  | .ownByID         => "own_by_id"
  | .ownByLegacyName => "own_by_legacy_name"
  | .shared          => "shared"
  | .other           => "other"
  | .noOwner         => "no_owner"
  | .openBranch      => "open_branch"
  | .unknownBranch   => "unknown_branch"

def Endpoint.toString : Endpoint → String
  | .healthz                => "healthz"
  | .readyz                 => "readyz"
  | .tlsPem                 => "tls_pem"
  | .authMethods            => "auth_methods"
  | .authLogin              => "auth_login"
  | .authSession            => "auth_session"
  | .authLogout             => "auth_logout"
  | .authPassword           => "auth_password"
  | .authReset              => "auth_reset"
  | .branchList             => "branch_list"
  | .branchGet              => "branch_get"
  | .branchDSN              => "branch_dsn"
  | .branchQuery            => "branch_query"
  | .branchCreate           => "branch_create"
  | .branchReset            => "branch_reset"
  | .branchDelete           => "branch_delete"
  | .branchExtend           => "branch_extend"
  | .branchPause            => "branch_pause"
  | .branchResume           => "branch_resume"
  | .branchCheckpointRead   => "branch_checkpoint_read"
  | .branchCheckpointCreate => "branch_checkpoint_create"
  | .branchCheckpointDelete => "branch_checkpoint_delete"
  | .branchRestore          => "branch_restore"
  | .branchUpgrade          => "branch_upgrade"
  | .branchFork             => "branch_fork"
  | .branchMerge            => "branch_merge"
  | .branchRecreate         => "branch_recreate"
  | .baseList               => "base_list"
  | .baseGet                => "base_get"
  | .basePublic             => "base_public"
  | .baseCreate             => "base_create"
  | .baseDescribe           => "base_describe"
  | .baseReplicas           => "base_replicas"
  | .basePatchAnonymous     => "base_patch_anonymous"
  | .baseDelete             => "base_delete"
  | .baseSnapshot           => "base_snapshot"
  | .baseMask               => "base_mask"
  | .baseStart              => "base_start"
  | .baseStop               => "base_stop"
  | .baseSQL                => "base_sql"
  | .baseLoadPath           => "base_load_path"
  | .baseLoadUpload         => "base_load_upload"
  | .baseRefreshRead        => "base_refresh_read"
  | .baseRefreshWrite       => "base_refresh_write"
  | .baseRefreshDelete      => "base_refresh_delete"
  | .baseRefreshRun         => "base_refresh_run"
  | .uploadCreate           => "upload_create"
  | .uploadRoom             => "upload_room"
  | .baseSnapshotPatch      => "base_snapshot_patch"
  | .baseSnapshotDelete     => "base_snapshot_delete"
  | .routeUserRead          => "route_user_read"
  | .routeUserWrite         => "route_user_write"
  | .routeUserDelete        => "route_user_delete"
  | .tokenRead              => "token_read"
  | .tokenWrite             => "token_write"
  | .tokenDelete            => "token_delete"
  | .accountPreferenceRead  => "account_preference_read"
  | .accountPreferenceWrite => "account_preference_write"
  | .accountTokenRead       => "account_token_read"
  | .accountTokenCreate     => "account_token_create"
  | .accountTokenDelete     => "account_token_delete"
  | .mcpUse                 => "mcp_use"
  | .settingRead            => "setting_read"
  | .settingWrite           => "setting_write"
  | .licenseRead            => "license_read"
  | .licenseVerify          => "license_verify"
  | .licenseWrite           => "license_write"
  | .licenseRenewalRead     => "license_renewal_read"
  | .licenseRenew           => "license_renew"
  | .moduleRead             => "module_read"
  | .moduleInstall          => "module_install"
  | .moduleRemove           => "module_remove"
  | .moduleForget           => "module_forget"
  | .updateRead             => "update_read"
  | .updateCheck            => "update_check"
  | .updateApply            => "update_apply"
  | .updateCancel           => "update_cancel"
  | .runtimeRead            => "runtime_read"
  | .runtimeAvailable       => "runtime_available"
  | .runtimeInstall         => "runtime_install"
  | .runtimeRemove          => "runtime_remove"
  | .operationList          => "operation_list"
  | .operationGet           => "operation_get"
  | .eventRead              => "event_read"
  | .capabilityRead         => "capability_read"
  | .clusterRead            => "cluster_read"
  | .nodeMetrics            => "node_metrics"
  | .nodeStorage            => "node_storage"
  | .metricsRead            => "metrics_read"
  | .clusterRemove          => "cluster_remove"
  | .clusterToken           => "cluster_token"
  | .backupRead             => "backup_read"
  | .backupCreate           => "backup_create"
  | .backupDownload         => "backup_download"
  | .backupRestore          => "backup_restore"
  | .extPrivateRead         => "ext_private_read"
  | .extPrivateWrite        => "ext_private_write"
  | .extPublicRead          => "ext_public_read"
  | .extPublicWrite         => "ext_public_write"

def DenyReason.toString : DenyReason → String
  | .ok                     => "ok"
  | .unauthenticated        => "unauthenticated"
  | .csrfFailed             => "csrf_failed"
  | .passwordChangeRequired => "password_change_required"
  | .forbidden              => "forbidden"
  | .managedElsewhere       => "managed_elsewhere"
  | .socketOnly             => "socket_only"
  | .invalidRequest         => "invalid_request"
  | .notFound               => "not_found"

def allEndpoints : List Endpoint :=
  [ .healthz, .readyz, .tlsPem, .authMethods
  , .authLogin, .authSession, .authLogout, .authPassword, .authReset
  , .branchList, .branchGet, .branchDSN, .branchQuery, .branchCreate
  , .branchReset, .branchDelete, .branchExtend, .branchPause, .branchResume
  , .branchCheckpointRead, .branchCheckpointCreate, .branchCheckpointDelete
  , .branchRestore, .branchUpgrade, .branchFork, .branchMerge, .branchRecreate
  , .baseList, .baseGet, .basePublic, .baseCreate, .baseDescribe, .baseReplicas
  , .basePatchAnonymous, .baseDelete, .baseSnapshot, .baseMask, .baseStart, .baseStop
  , .baseSQL, .baseLoadPath, .baseLoadUpload, .baseRefreshRead, .baseRefreshWrite
  , .baseRefreshDelete, .baseRefreshRun, .uploadCreate, .uploadRoom
  , .baseSnapshotPatch, .baseSnapshotDelete
  , .routeUserRead, .routeUserWrite, .routeUserDelete
  , .tokenRead, .tokenWrite, .tokenDelete
  , .accountPreferenceRead, .accountPreferenceWrite
  , .accountTokenRead, .accountTokenCreate, .accountTokenDelete, .mcpUse
  , .settingRead, .settingWrite
  , .licenseRead, .licenseVerify, .licenseWrite, .licenseRenewalRead, .licenseRenew
  , .moduleRead, .moduleInstall, .moduleRemove, .moduleForget
  , .updateRead, .updateCheck, .updateApply, .updateCancel
  , .runtimeRead, .runtimeAvailable, .runtimeInstall, .runtimeRemove
  , .operationList, .operationGet, .eventRead, .capabilityRead
  , .clusterRead, .nodeMetrics, .nodeStorage, .metricsRead, .clusterRemove, .clusterToken
  , .backupRead, .backupCreate, .backupDownload, .backupRestore
  , .extPrivateRead, .extPrivateWrite, .extPublicRead, .extPublicWrite ]

def allOwnerRels : List OwnerRel :=
  [ .ownByID, .ownByLegacyName, .shared, .other, .noOwner, .openBranch, .unknownBranch ]

def allSessStates : List SessState :=
  [ .valid, .mustChange, .csrfBad, .expired, .revoked, .wrongGen, .localUserSession ]

def sessionCheckEndpoints : List Endpoint :=
  [ .healthz, .readyz, .tlsPem, .authMethods, .authLogin, .authSession, .authLogout, .authPassword, .authReset
  , .branchList, .branchGet, .branchCreate, .branchDelete
  , .settingRead, .settingWrite, .clusterToken, .backupRestore
  , .accountTokenCreate, .baseLoadPath, .baseLoadUpload
  , .extPublicRead, .extPublicWrite, .extPrivateRead, .extPrivateWrite ]

def endpointRelPairs : List (Endpoint × OwnerRel) :=
  allEndpoints.flatMap fun ep =>
    if ep.hasBranchName then
      allOwnerRels.map (fun rel => (ep, rel))
    else
      [(ep, .other)]

def enumerateRequests : List AuthRequest :=
  let socketReqs := endpointRelPairs.map fun (ep, rel) =>
    { channel := .socket, authKind := .none, role := .localPrincipal, isTokenID := false, sessState := .valid, endpoint := ep, ownerRel := rel }
  let fwdMissingHdrs := [Endpoint.healthz, .authMethods, .branchList, .authReset].map fun ep =>
    { channel := .nodeForward, authKind := .missingForwardHeaders, role := .emptyRole, isTokenID := false, sessState := .valid, endpoint := ep, ownerRel := .other }
  let fwdMalformed := allEndpoints.map fun ep =>
    { channel := .nodeForward, authKind := .malformedForward, role := .emptyRole, isTokenID := false, sessState := .valid, endpoint := ep, ownerRel := .other }
  let fwdPrincipals : List (Role × Bool) :=
    [ (.localPrincipal, false), (.admin, false), (.ssoAdmin, false), (.scopeFull, true), (.scopeBranches, true), (.scopeBranches, false), (.viewer, false), (.anonymous, false), (.emptyRole, false) ]
  let fwdReqs := fwdPrincipals.flatMap fun (r, tok) =>
    endpointRelPairs.map fun (ep, rel) =>
      { channel := .nodeForward, authKind := .bearerToken, role := r, isTokenID := tok, sessState := .valid, endpoint := ep, ownerRel := rel }
  let httpAnon := endpointRelPairs.map fun (ep, rel) =>
    { channel := .publicHTTP, authKind := .none, role := .anonymous, isTokenID := false, sessState := .valid, endpoint := ep, ownerRel := rel }
  let httpBearers : List (Role × Bool) :=
    [ (.localPrincipal, false), (.admin, false), (.ssoAdmin, false), (.scopeFull, true), (.scopeBranches, true), (.scopeBranches, false), (.viewer, false), (.anonymous, false), (.emptyRole, false) ]
  let httpBearerReqs := httpBearers.flatMap fun (r, tok) =>
    endpointRelPairs.map fun (ep, rel) =>
      { channel := .publicHTTP, authKind := .bearerToken, role := r, isTokenID := tok, sessState := .valid, endpoint := ep, ownerRel := rel }
  let httpSessionViewer := endpointRelPairs.map fun (ep, rel) =>
    { channel := .publicHTTP, authKind := .sessionCookie, role := .viewer, isTokenID := false, sessState := .valid, endpoint := ep, ownerRel := rel }
  let httpSessionSSOAdmin := sessionCheckEndpoints.map fun ep =>
    { channel := .publicHTTP, authKind := .sessionCookie, role := .ssoAdmin, isTokenID := false, sessState := .valid, endpoint := ep, ownerRel := .other }
  let httpSessionStates := allSessStates.flatMap fun st =>
    sessionCheckEndpoints.map fun ep =>
      { channel := .publicHTTP, authKind := .sessionCookie, role := .admin, isTokenID := false, sessState := st, endpoint := ep, ownerRel := .other }
  socketReqs ++ fwdMissingHdrs ++ fwdMalformed ++ fwdReqs ++ httpAnon ++ httpBearerReqs ++ httpSessionViewer ++ httpSessionSSOAdmin ++ httpSessionStates

def jsonBool (b : Bool) : String := if b then "true" else "false"

def jsonStr (s : String) : String := "\"" ++ s ++ "\""

def encodeHTTPRow (req : AuthRequest) : String :=
  let dec := evalRequest req
  "{" ++
  "\"channel\":" ++ jsonStr (Channel.toString req.channel) ++ "," ++
  "\"auth_kind\":" ++ jsonStr (AuthKind.toString req.authKind) ++ "," ++
  "\"role\":" ++ jsonStr (Role.toString req.role) ++ "," ++
  "\"is_token_id\":" ++ jsonBool req.isTokenID ++ "," ++
  "\"sess_state\":" ++ jsonStr (SessState.toString req.sessState) ++ "," ++
  "\"endpoint\":" ++ jsonStr (Endpoint.toString req.endpoint) ++ "," ++
  "\"ownership\":" ++ jsonStr (OwnerRel.toString req.ownerRel) ++ "," ++
  "\"allowed\":" ++ jsonBool dec.allowed ++ "," ++
  "\"deny_reason\":" ++ jsonStr (DenyReason.toString dec.reason) ++
  "}"

def Plan.toString : Plan → String
  | .lite        => "lite"
  | .team        => "team"
  | .business    => "business"
  | .startup     => "startup"
  | .growth      => "growth"
  | .enterprise  => "enterprise"
  | .emptyPlan   => ""
  | .unknownPlan => "unknown"

def Feature.toString : Feature → String
  | .users   => "users"
  | .audit   => "audit"
  | .sso     => "sso"
  | .masking => "masking"
  | .history => "history"
  | .diff    => "diff"

def LicenseState.toString : LicenseState → String
  | .valid     => "valid"
  | .grace     => "grace"
  | .expired   => "expired"
  | .overNodes => "over_nodes"

def allPlans : List Plan :=
  [.lite, .team, .business, .startup, .growth, .enterprise, .emptyPlan, .unknownPlan]

def allFeatures : List Feature :=
  [.users, .audit, .sso, .masking, .history, .diff]

def moduleSets : List (List String) :=
  [[], ["*"], ["masking"], ["diff"], ["masking", "diff"]]

def encodeLicenseHasCases : List String :=
  allPlans.flatMap fun p =>
    moduleSets.flatMap fun mods =>
      allFeatures.map fun f =>
        let l : License := { plan := p, modules := mods, maxNodes := 0, expires := 10, graceDays := 0 }
        let modsJson := "[" ++ String.intercalate "," (mods.map jsonStr) ++ "]"
        "{" ++
        "\"plan\":" ++ jsonStr (Plan.toString p) ++ "," ++
        "\"modules\":" ++ modsJson ++ "," ++
        "\"feature\":" ++ jsonStr (Feature.toString f) ++ "," ++
        "\"has\":" ++ jsonBool (License.has l f) ++
        "}"

def encodeLicenseStateCases : List String :=
  [0, 3].flatMap fun maxN =>
    [2, 3, 4].flatMap fun nodes =>
      [9, 10, 23, 24].map fun now =>
        let l : License := { plan := .lite, modules := ["history"], maxNodes := maxN, expires := 10, graceDays := 14 }
        let st := License.state l now nodes
        "{" ++
        "\"max_nodes\":" ++ toString maxN ++ "," ++
        "\"nodes\":" ++ toString nodes ++ "," ++
        "\"now_day\":" ++ toString now ++ "," ++
        "\"expires_day\":10," ++
        "\"grace_days\":14," ++
        "\"state\":" ++ jsonStr (LicenseState.toString st) ++ "," ++
        "\"usable\":" ++ jsonBool (LicenseState.usable st) ++
        "}"

def encodeBranchReadCases : List String :=
  let principals : List (Role × Bool) :=
    [(.admin, false), (.ssoAdmin, false), (.scopeFull, true), (.scopeBranches, true), (.scopeBranches, false), (.viewer, false), (.anonymous, false), (.emptyRole, false)]
  let rels : List OwnerRel := [.ownByID, .ownByLegacyName, .shared, .other, .noOwner, .openBranch]
  principals.flatMap fun (r, isTok) =>
    rels.map fun rel =>
      "{" ++
      "\"role\":" ++ jsonStr (Role.toString r) ++ "," ++
      "\"is_token_id\":" ++ jsonBool isTok ++ "," ++
      "\"ownership\":" ++ jsonStr (OwnerRel.toString rel) ++ "," ++
      "\"can_read\":" ++ jsonBool (canReadBranch r isTok rel) ++
      "}"

def buildMatrixJSON : String :=
  let httpRows := enumerateRequests.map encodeHTTPRow
  let branchReads := encodeBranchReadCases
  let licHas := encodeLicenseHasCases
  let licStates := encodeLicenseStateCases
  "{\n" ++
  "  \"http_cases\": [\n    " ++ String.intercalate ",\n    " httpRows ++ "\n  ],\n" ++
  "  \"branch_read_cases\": [\n    " ++ String.intercalate ",\n    " branchReads ++ "\n  ],\n" ++
  "  \"license_has_cases\": [\n    " ++ String.intercalate ",\n    " licHas ++ "\n  ],\n" ++
  "  \"license_state_cases\": [\n    " ++ String.intercalate ",\n    " licStates ++ "\n  ]\n" ++
  "}\n"

end DbBranchAuthz.JsonExport
