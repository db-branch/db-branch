// How modules and licence plans read in the console.

// Plans, as the server reads them (keys of the earlier plans arrive as
// the plan that replaced them).
const plans: Record<string, [string, string]> = {
  lite: ["Lite", "history, diff"],
  team: ["Team", "users, roles, teams, history, diff"],
  business: ["Business", "users, roles, teams, SSO, audit"],
};

export function planLabel(plan?: string): string {
  const p = plans[plan || "business"];
  return p ? `${p[0]} · ${p[1]}` : (plan ?? "");
}

// moduleLabel is how a module reads in the sidebar and headings; the module name itself is unchanged.
const moduleLabels: Record<string, string> = { audit: "Audit", auth: "Auth", masking: "Masking", proxysql: "ProxySQL" };
export function moduleLabel(name: string): string {
  return moduleLabels[name] ?? name.charAt(0).toUpperCase() + name.slice(1);
}
