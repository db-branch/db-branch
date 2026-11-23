import { describe, expect, it } from "vitest";
import type { Base, Branch } from "../api";
import { activeCount, filterBases, filterBranches, optionsOf, parseFilter, toSearch } from "./filter";

const br = (name: string, extra: Partial<Branch> = {}): Branch =>
  ({ name, id: name, status: "ready", status_reason: null, base: "orders", snapshot: "s1", runtime: "mysql@8.4.13", node: "n1", owner: "alice", access: "private", ...extra }) as Branch;
const ba = (name: string, extra: Partial<Base> = {}): Base => ({ name, status: "ready", status_reason: null, runtime: "mysql@8.4.13", node: "n1", description: "", ...extra }) as Base;

describe("parseFilter and toSearch", () => {
  it("reads the filter from a query string and writes only what is set", () => {
    expect(parseFilter("?q=feature&status=failed&x=1", ["q", "status", "base"])).toEqual({ q: "feature", status: "failed", base: "" });
    expect(parseFilter("", ["q", "status"])).toEqual({ q: "", status: "" });
    expect(toSearch({ q: "a b", status: "", base: "orders" })).toBe("?q=a+b&base=orders");
    expect(toSearch({ q: "", status: "" })).toBe("");
  });
  it("round-trips", () => {
    const f = { q: "x&y=z", status: "ready" };
    expect(parseFilter(toSearch(f), ["q", "status"])).toEqual(f);
  });
});

describe("activeCount", () => {
  it("counts the filters that are set", () => {
    expect(activeCount({ q: "", status: "" })).toBe(0);
    expect(activeCount({ q: " ", status: "" })).toBe(0);
    expect(activeCount({ q: "a", status: "ready" })).toBe(2);
  });
});

describe("filterBranches", () => {
  const list = [
    br("alice-report", { owner: "alice", base: "orders", status: "ready" }),
    br("bob-fix", { owner: "bob", base: "shop", status: "failed", status_reason: "no space left", runtime: "postgres@18.6" }),
    br("carol-x", { owner: "carol", base: "orders", status: "provisioning", access: "open" }),
  ];
  const f = (over: Partial<Record<string, string>>) => filterBranches(list, { q: "", status: "", engine: "", base: "", owner: "", ...over }).map((b) => b.name);
  it("keeps all for an empty filter", () => expect(f({})).toHaveLength(3));
  it("searches name, owner, base, runtime and the reason, every word having to match", () => {
    expect(f({ q: "REPORT" })).toEqual(["alice-report"]);
    expect(f({ q: "bob" })).toEqual(["bob-fix"]);
    expect(f({ q: "shop" })).toEqual(["bob-fix"]);
    expect(f({ q: "postgres" })).toEqual(["bob-fix"]);
    expect(f({ q: "space" })).toEqual(["bob-fix"]);
    expect(f({ q: "orders ready" })).toEqual(["alice-report"]);
    expect(f({ q: "nothing" })).toEqual([]);
  });
  it("narrows by status, engine, base and owner together", () => {
    expect(f({ status: "failed" })).toEqual(["bob-fix"]);
    expect(f({ engine: "mysql" })).toEqual(["alice-report", "carol-x"]);
    expect(f({ base: "orders", owner: "carol" })).toEqual(["carol-x"]);
    expect(f({ base: "orders", status: "failed" })).toEqual([]);
  });
  it("matches owner_name as well as immutable owner id", () => {
    const withId = [
      br("alice-report", { owner: "sso:alice", owner_name: "alice@example.com" }),
      br("ci-run", { owner: "token:abc123", owner_name: "ci" }),
    ];
    const run = (over: Partial<Record<string, string>>) => filterBranches(withId, { q: "", status: "", engine: "", base: "", owner: "", ...over }).map((b) => b.name);
    expect(run({ owner: "alice@example.com" })).toEqual(["alice-report"]);
    expect(run({ owner: "sso:alice" })).toEqual(["alice-report"]);
    expect(run({ q: "example.com" })).toEqual(["alice-report"]);
    expect(run({ q: "token:abc123" })).toEqual(["ci-run"]);
  });
  it("matches parent_branch in search queries", () => {
    const forked = [br("parent"), br("child", { parent_branch: "parent" }), br("other")];
    const run = (q: string) => filterBranches(forked, { q, status: "", engine: "", base: "", owner: "" }).map((b) => b.name);
    expect(run("parent")).toEqual(["parent", "child"]);
  });
});

describe("filterBases", () => {
  const list = [ba("orders", { description: "Production copy" }), ba("shop", { runtime: "postgres@18.6", status: "failed", node: "n2" }), ba("old", { status: "ready", runtime: "mysql@5.7.44" })];
  const f = (over: Partial<Record<string, string>>) => filterBases(list, { q: "", status: "", engine: "", node: "", ...over }).map((b) => b.name);
  it("searches name, description, runtime and node, and narrows by status, engine and node", () => {
    expect(f({ q: "production" })).toEqual(["orders"]);
    expect(f({ q: "5.7" })).toEqual(["old"]);
    expect(f({ status: "failed" })).toEqual(["shop"]);
    expect(f({ engine: "postgres" })).toEqual(["shop"]);
    expect(f({ node: "n2" })).toEqual(["shop"]);
    expect(f({})).toHaveLength(3);
  });
});

describe("optionsOf", () => {
  it("lists the distinct values, sorted, without the empty ones", () => {
    expect(optionsOf(["ready", "failed", "ready", "", undefined as unknown as string])).toEqual(["failed", "ready"]);
  });
});
