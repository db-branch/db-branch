// How a subscription licence and its automatic renewal read in the console.

/** The leader's view of renewing the saved key (GET /v1/license, "renewal"). */
export type Renewal = {
  enabled: boolean;
  last_attempt?: string;
  last_result?: "renewed" | "not_yet" | "inactive" | "error" | "unknown";
  message?: string;
  next_attempt?: string;
};

/** billingLabel: "Monthly" / "Yearly"; "" for a key bought once. */
export function billingLabel(billing?: string): string {
  return billing === "monthly" ? "Monthly" : billing === "yearly" ? "Yearly" : "";
}

/** resultLabel is the English text (translated by the caller) for a renewal result. */
export function resultLabel(result?: string): string {
  switch (result) {
    case "renewed":
      return "Renewed";
    case "not_yet":
      return "No newer key yet";
    case "inactive":
      return "Subscription not active";
    case "error":
      return "Failed";
    case "unknown":
      return "Unknown";
    default:
      return "";
  }
}

export type RenewalInfo = {
  /** The key can be renewed online (it carries a subscription). */
  subscribed: boolean;
  /** license.auto_renew is on for such a key. */
  auto: boolean;
  /** Show "Check now". */
  canCheck: boolean;
  next?: string;
  last?: string;
  result?: string;
  message?: string;
  /** The last attempt failed or the subscription is not active. */
  warn: boolean;
};

/**
 * renewalInfo decides what the License card shows. mayCheck: the user may
 * change the licence (the console hides the button otherwise).
 */
export function renewalInfo(license: { subscription?: string }, renewal: Renewal | undefined, mayCheck: boolean): RenewalInfo {
  const subscribed = !!license.subscription;
  const auto = subscribed && (renewal?.enabled ?? true);
  const result = renewal?.last_result || undefined;
  return {
    subscribed,
    auto,
    canCheck: auto && mayCheck,
    next: auto ? renewal?.next_attempt : undefined,
    last: renewal?.last_attempt,
    result,
    message: renewal?.message,
    warn: result === "inactive" || result === "error",
  };
}
