// Keeping the demo's data while someone moves around inside one tab (switching
// between the admin console and the user page loads a new page), but not when
// they reload: a reload starts the demo over, as the banner says.
export const KEY = "db-branch.demo.state.v1";

/** shouldRestore: use the saved state unless the page was reloaded (a language switch reloads on purpose and asks to keep it). */
export function shouldRestore(saved: string | null, navType: string | undefined): boolean {
  if (!saved) return false;
  if (navType !== "reload") return true;
  try {
    return (JSON.parse(saved) as { keep?: boolean }).keep === true;
  } catch {
    return false;
  }
}
