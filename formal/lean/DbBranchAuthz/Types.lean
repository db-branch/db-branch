namespace DbBranchAuthz

/-- Request entry channel into the db-branch agent. -/
inductive Channel where
  | socket      -- Local UNIX socket (Handler(true), no forwarding headers)
  | nodeForward -- Node-to-node mTLS /internal/v1/api/ (Handler(true), forwarded headers)
  | publicHTTP  -- Public/TLS HTTP listener (Handler(false))
  deriving DecidableEq, Repr, Inhabited

/-- Authentication mechanism presented on the request. -/
inductive AuthKind where
  | none                  -- No credentials (or unauthenticated on HTTP)
  | sessionCookie         -- Console dbb_session cookie
  | bearerToken           -- Authorization: Bearer <token>
  | missingForwardHeaders -- NodeForward missing/empty/duplicate forwarding headers (rejected by nodeapi.forwardedAPI)
  | malformedForward      -- NodeForward with valid outer forwarding headers, invalid X-Db-Branch-Principal JSON
  deriving DecidableEq, Repr, Inhabited

/-- Principal role / identity model. -/
inductive Role where
  | localPrincipal -- LocalPrincipal (ID = "local", Roles = ["admin"])
  | admin          -- Built-in "admin" user (ID = "admin", Roles = ["admin"])
  | ssoAdmin       -- External SSO/auth user ("sso:admin-user") with ["admin"] (not built-in "admin")
  | scopeFull      -- "scope:full" API token
  | scopeBranches  -- "scope:branches" API token (service or personal)
  | viewer         -- External module non-admin role (e.g. "viewer")
  | anonymous      -- Anonymous principal (ID = "anonymous")
  | emptyRole      -- Authenticated principal with empty/other roles (Roles = [])
  deriving DecidableEq, Repr, Inhabited

/-- State of the console session (meaningful for AuthKind.sessionCookie). -/
inductive SessState where
  | valid            -- Active session, CSRF matches (or GET)
  | mustChange       -- Initial password session (MustChange = true)
  | csrfBad          -- Non-GET request with missing/wrong X-CSRF-Token
  | expired          -- Session past ExpiresAt
  | revoked          -- Session ID in Revocations / s.revoked
  | wrongGen         -- Credentials.Generation changed since issue
  | localUserSession -- Session claiming User == "local"
  deriving DecidableEq, Repr, Inhabited

/-- Relationship between the acting principal and the target branch (for named branch actions). -/
inductive OwnerRel where
  | ownByID         -- Branch owned by p.ID
  | ownByLegacyName -- Legacy unmigrated branch (OwnerName == "", Owner == p.Name, not "token:")
  | shared          -- Branch owned by another user who SharesBranches with p
  | other           -- Private branch owned by another user, not shared
  | noOwner         -- Legacy branch with Owner == "" (admin-only unless open)
  | openBranch      -- Branch with Access == "open" owned by another user
  | unknownBranch   -- Branch does not exist (404 vs 403)
  deriving DecidableEq, Repr, Inhabited

/-- HTTP API endpoints modeled from all routes in internal/interface/httpapi/server.go and ext.go. -/
inductive Endpoint where
  | healthz
  | readyz
  | tlsPem
  | authMethods
  | authLogin
  | authSession
  | authLogout
  | authPassword
  | authReset
  | userList
  | userCreate
  | userGet
  | userPatch
  | userResetPassword
  | userDelete
  | branchList
  | branchGet
  | branchDSN
  | branchQuery
  | branchCreate
  | branchReset
  | branchDelete
  | branchExtend
  | branchPause
  | branchResume
  | branchCheckpointRead
  | branchCheckpointCreate
  | branchCheckpointDelete
  | branchRestore
  | branchUpgrade
  | branchFork
  | branchMerge
  | branchRecreate
  | baseList
  | baseGet
  | basePublic
  | baseCreate
  | baseDescribe
  | baseReplicas
  | basePatchAnonymous
  | baseDelete
  | baseSnapshot
  | baseMask
  | baseStart
  | baseStop
  | baseSQL
  | baseLoadPath
  | baseLoadUpload
  | baseRefreshRead
  | baseRefreshWrite
  | baseRefreshDelete
  | baseRefreshRun
  | uploadCreate
  | uploadRoom
  | baseSnapshotPatch
  | baseSnapshotDelete
  | routeUserRead
  | routeUserWrite
  | routeUserDelete
  | tokenRead
  | tokenWrite
  | tokenDelete
  | accountPreferenceRead
  | accountPreferenceWrite
  | accountTokenRead
  | accountTokenCreate
  | accountTokenDelete
  | mcpUse
  | settingRead
  | settingWrite
  | licenseRead
  | licenseVerify
  | licenseWrite
  | licenseRenewalRead
  | licenseRenew
  | moduleRead
  | moduleInstall
  | moduleRemove
  | moduleForget
  | updateRead
  | updateCheck
  | updateApply
  | updateCancel
  | runtimeRead
  | runtimeAvailable
  | runtimeInstall
  | runtimeRemove
  | operationList
  | operationGet
  | eventRead
  | capabilityRead
  | clusterRead
  | nodeMetrics
  | nodeStorage
  | metricsRead
  | clusterRemove
  | clusterToken
  | backupRead
  | backupCreate
  | backupDownload
  | backupRestore
  | extPrivateRead
  | extPrivateWrite
  | extPublicRead
  | extPublicWrite
  deriving DecidableEq, Repr, Inhabited

/-- Outcome / denial reason aligned with HTTP status & error code. -/
inductive DenyReason where
  | ok
  | unauthenticated        -- 401
  | csrfFailed             -- 403 csrf_failed
  | passwordChangeRequired -- 403 password_change_required
  | forbidden              -- 403 forbidden (or nodeapi forwarded headers required)
  | managedElsewhere       -- 403 managed_elsewhere (non-built-in-admin without PasswordChanger)
  | socketOnly             -- 403 socket_only
  | invalidRequest         -- 400 invalid_request (e.g. CreatePersonal / resolveDump remote path)
  | notFound               -- 404 (Authorize passed, handler reports unknown branch)
  deriving DecidableEq, Repr, Inhabited

/-- Full request input for authorization evaluation. -/
structure AuthRequest where
  channel   : Channel
  authKind  : AuthKind
  role      : Role
  isTokenID : Bool      -- True when Principal.ID starts with "token:" (service token)
  sessState : SessState
  endpoint  : Endpoint
  ownerRel  : OwnerRel
  deriving DecidableEq, Repr, Inhabited

structure Decision where
  allowed : Bool
  reason  : DenyReason
  deriving DecidableEq, Repr, Inhabited

def Decision.allow : Decision := ⟨true, .ok⟩
def Decision.deny (r : DenyReason) : Decision := ⟨false, r⟩

end DbBranchAuthz
