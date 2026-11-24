import { language, t } from "./i18n";
import { ja } from "./ja";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

export const UNKNOWN_ERROR_TEXT = "An error occurred.";

/**
 * ERROR_CODES maps server and transport error codes to canonical English
 * sentences (all translated in ja.ts).
 */
export const ERROR_CODES: Record<string, string> = {
  already_exists: "Already exists.",
  audit_unavailable: "The change cannot be recorded in the audit log, so it was not made.",
  auto_renew_off: "Automatic renewal is off.",
  cannot_verify: "This node cannot verify the licence key.",
  checkpoints_unavailable: "Checkpoints are not available on this storage.",
  cluster_upgrading: "The cluster is upgrading. Try again after the upgrade finishes.",
  conflict: "This action conflicts with the current state.",
  csrf_failed: "Request verification failed. Reload the page and try again.",
  feature_not_supported: "This feature is not supported.",
  forbidden: "You do not have permission to do this.",
  in_progress: "Another operation is already in progress.",
  in_use: "In use and cannot be changed or deleted.",
  internal: "An internal server error occurred. See the db-branch agent log on the node.",
  invalid_credentials: "Wrong user name or password.",
  invalid_license: "The licence key is invalid.",
  invalid_request: "The request or input is invalid.",
  length_required: "Content-Length is required for the upload.",
  license_unusable: "This licence key cannot be used.",
  logout_not_recorded: "The sign-out could not be recorded on every node. Your session is still open; try again.",
  managed_elsewhere: "This account's password is managed by your sign-in provider.",
  merge_conflict: "Merge conflicts were detected.",
  merge_failed: "The merge failed.",
  modules_pending: "Some modules are not ready yet. Try again shortly.",
  network: "The connection was interrupted. Check your network and try again.",
  aborted: "The operation was cancelled.",
  no_capacity: "Not enough memory or capacity is available.",
  no_common_ancestor: "No common ancestor was found to merge from.",
  no_leader: "No cluster leader is available. Try again shortly.",
  no_proxy: "This node has no ProxySQL configured.",
  no_space: "Not enough free disk or ZFS pool space.",
  not_clustered: "This node is not in a cluster.",
  not_found: "Not found.",
  not_implemented: "This feature is not available on this node.",
  not_initialized: "This node is not initialized yet.",
  not_renewable: "This licence key does not support automatic renewal.",
  not_routable: "This branch cannot be reached through ProxySQL.",
  other_node: "Run this action on the node holding the resource.",
  password_change_required: "Set a new password first.",
  query_failed: "The SQL query failed.",
  quota_exceeded: "Limit reached; cannot create more.",
  rate_limited: "Too many requests. Wait a moment and try again.",
  restore_failed: "The restore failed.",
  rollout_running: "Another update rollout is already running.",
  runtime_not_installed: "The engine version is not installed on this node.",
  runtime_unavailable: "The engine version is unavailable.",
  schema_no_primary_key: "A changed table has no primary key or NOT NULL unique constraint.",
  socket_only: "This action can only be run through the local socket on the node.",
  too_large: "The file or request is too large.",
  triggers_not_supported: "Tables with triggers cannot be merged.",
  unauthenticated: "Authentication is required. Sign in again.",
  unclean_shutdown: "The server did not shut down cleanly.",
  upgrade_not_allowed: "This upgrade path is not allowed.",
  weak_password: "The password is too weak. Use at least 12 characters.",
};

/**
 * LocalizedError marks a client-side error whose message is already translated
 * via t() and must not be wrapped with the generic fallback error text.
 */
export class LocalizedError extends Error {}

/**
 * serverError wraps a raw message returned by an asynchronous server payload
 * (e.g. Operation.error or MergeResult.error) so formatError translates known
 * codes or falls back to generic text + small raw detail in Japanese mode.
 */
export function serverError(message: string, code = "unknown"): ApiError {
  return new ApiError(0, code, message);
}

export type FormattedError = { text: string; detail?: string };

/**
 * formatError formats an error for display in the active console language.
 * In Japanese mode, ApiError codes are translated via ERROR_CODES; unknown
 * codes and generic errors fall back to a generic Japanese sentence with the
 * original message in small text.
 */
export function formatError(err: unknown): FormattedError | null {
  if (err === null || err === undefined || err === "") return null;
  if (typeof err === "string") {
    if (language() !== "ja") return { text: err };
    if (err in ja) return { text: ja[err] };
    if (!/[A-Za-z]/.test(err) || /[\u3040-\u30ff\u4e00-\u9faf]/.test(err)) return { text: err };
    return { text: t(UNKNOWN_ERROR_TEXT), detail: err };
  }
  if (err instanceof LocalizedError) {
    return { text: err.message };
  }
  if (err instanceof ApiError) {
    if (language() !== "ja") return { text: err.message };
    const canonical = ERROR_CODES[err.code];
    if (canonical) {
      const text = t(canonical);
      return err.code === "query_failed" && err.message ? { text, detail: err.message } : { text };
    }
    if (err.message && err.message in ja) return { text: ja[err.message] };
    return { text: t(UNKNOWN_ERROR_TEXT), detail: err.message || undefined };
  }
  if (err instanceof Error) {
    if (language() !== "ja") return { text: err.message };
    if (err.message && err.message in ja) return { text: ja[err.message] };
    return { text: t(UNKNOWN_ERROR_TEXT), detail: err.message || undefined };
  }
  return { text: String(err) };
}

/** errorText returns a single-line string representation of formatError(err). */
export function errorText(err: unknown): string {
  const f = formatError(err);
  if (!f) return "";
  return f.detail ? `${f.text} (${f.detail})` : f.text;
}
