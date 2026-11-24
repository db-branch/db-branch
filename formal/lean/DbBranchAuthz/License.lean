namespace DbBranchAuthz.License

/-- Model of pkg/ext/license/license.go. -/

inductive Plan where
  | lite
  | team
  | business
  | startup
  | growth
  | enterprise
  | emptyPlan
  | unknownPlan
  deriving DecidableEq, Repr, Inhabited

inductive Feature where
  | users
  | audit
  | sso
  | masking
  | history
  | diff
  deriving DecidableEq, Repr, Inhabited

inductive LicenseState where
  | valid
  | grace
  | expired
  | overNodes
  deriving DecidableEq, Repr, Inhabited

structure License where
  plan      : Plan
  modules   : List String
  maxNodes  : Nat -- 0 = unlimited
  expires   : Nat -- unix days/seconds
  graceDays : Nat -- grace period length
  deriving DecidableEq, Repr

/-- earlierPlans + currentPlan in pkg/ext/license/license.go:111-118. -/
def currentPlan : Plan → Plan
  | .startup    => .team
  | .growth     => .team
  | .enterprise => .business
  | .emptyPlan  => .business
  | p           => p

/-- planFeatures in pkg/ext/license/license.go:94-98. -/
def planFeatures : Plan → List Feature
  | .lite     => [.history]
  | .team     => [.users, .history]
  | .business => [.users, .audit, .sso, .history]
  | _         => []

/-- optIn in pkg/ext/license/license.go:105. -/
def optInModule : Feature → Option String
  | .masking => some "masking"
  | .diff    => some "diff"
  | _        => none

/-- License.Has in pkg/ext/license/license.go:128-138. -/
def License.has (l : License) (f : Feature) : Bool :=
  match optInModule f with
  | some m => l.modules.contains m
  | none   => (planFeatures (currentPlan l.plan)).contains f

/-- License.Covers in pkg/ext/license/license.go:151-158. -/
def License.covers (l : License) (module : String) : Bool :=
  l.modules.any (fun m => m == module || m == "*")

/-- License.State in pkg/ext/license/license.go:174-184. -/
def License.state (l : License) (now : Nat) (nodes : Nat) : LicenseState :=
  if l.maxNodes > 0 && nodes > l.maxNodes then
    .overNodes
  else if now < l.expires then
    .valid
  else if now < l.expires + l.graceDays then
    .grace
  else
    .expired

/-- State.Usable in pkg/ext/license/license.go:187. -/
def LicenseState.usable : LicenseState → Bool
  | .valid | .grace => true
  | .expired | .overNodes => false

/-- Trial license in pkg/ext/license/license.go:146-148. -/
def trialLicense (start : Nat) : License :=
  { plan := .business, modules := ["*"], maxNodes := 0, expires := start + 30, graceDays := 0 }

end DbBranchAuthz.License
