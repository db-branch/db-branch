import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it } from "vitest";
import { api, type Branch, type Session } from "./api";
import { visiblePages } from "./App";
import { BranchRow } from "./components/branchcard";
import { BranchView } from "./components/branchview";
import { buildNav } from "./lib/nav";
import { Bases } from "./pages/Bases";
import { Branches } from "./pages/Branches";

const noop = () => {};

const maintainerSession: Session = {
  user: { id: "u-maint", name: "maint", roles: ["maintainer"] },
  csrf_token: "csrf",
  can: {
    "branch.read": true,
    "branch.create": false,
    "branch.extend": false,
    "branch.pause": false,
    "branch.resume": false,
    "branch.reset": false,
    "branch.delete": false,
    "branch.recreate": false,
    "branch.connect": false,
    "branch.query": false,
    "base.read": true,
    "base.create": true,
    "cluster.read": true,
    "setting.write": false,
  },
};

const sampleBranch: Branch = {
  name: "alice-feature-x",
  status: "lost",
  status_reason: null,
  base: "orders",
  snapshot: "s20261112",
  runtime: "mysql@8.4.13",
  node: "node1",
  created_at: "2026-11-12T10:00:00Z",
  expires_at: "2026-11-19T10:00:00Z",
  reset_at: null,
  storage: { used: 1024, referenced: 2048 },
  connection: { host: "10.0.0.1", port: 40001, user: "alice" },
  proxy: { host: "proxy", port: 6033, user: "app" },
  operation: null,
  owner: "u-alice",
  owner_name: "alice",
  id: "alice/feature-x",
  access: "private",
  checkpoints: true,
};

async function setSession(s: Session) {
  const origFetch = globalThis.fetch;
  globalThis.fetch = (async () => new Response(JSON.stringify(s), { status: 200 })) as typeof fetch;
  try {
    await api.session();
  } finally {
    globalThis.fetch = origFetch;
  }
}

describe("TestApp_MaintainerにBranchesが表示され権限のないボタンが隠れる", () => {
  const origLocation = globalThis.location;

  afterEach(() => {
    globalThis.location = origLocation;
    // Reset cached session to unauthenticated default.
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async () => new Response("{}", { status: 401 })) as typeof fetch;
    return api.session().catch(() => {}).finally(() => {
      globalThis.fetch = origFetch;
    });
  });

  it("Maintainer のナビゲーションに Branches が表示される", async () => {
    await setSession(maintainerSession);
    const pages = visiblePages();
    expect(pages).toContain("branches");
    expect(pages).toContain("bases");
    expect(pages).toContain("cluster");
    expect(pages).not.toContain("settings");

    const nav = buildNav({ pages, canSettings: false, enabled: undefined, modPages: [] });
    expect(nav.map((n) => n.id)).toContain("page:branches");
  });

  it("権限のないアクションボタンを BranchView / Branches / Bases / BranchRow で隠す", async () => {
    globalThis.location = { search: "", pathname: "/admin" } as Location;
    await setSession(maintainerSession);
    const viewHtml = renderToStaticMarkup(
      <BranchView
        b={{ ...sampleBranch, status: "ready" }}
        clustered={false}
        max={2048}
        allow={(a) => !!maintainerSession.can?.[a]}
        onBack={noop}
        onExtend={noop}
        onReset={noop}
        onRecreate={noop}
        onDelete={noop}
        onCopyProxy={noop}
        onPause={noop}
        onResume={noop}
      />,
    );
    for (const hidden of ["Copy connection URL", "Copy proxy URL", "Pause", "Extend 1h", "Extend 24h", "Extend 7d", "Reset", "Delete"]) {
      expect(viewHtml).not.toContain(hidden);
    }

    const lostHtml = renderToStaticMarkup(
      <BranchView
        b={sampleBranch}
        clustered={false}
        max={2048}
        allow={(a) => !!maintainerSession.can?.[a]}
        onBack={noop}
        onExtend={noop}
        onReset={noop}
        onRecreate={noop}
        onDelete={noop}
        onCopyProxy={noop}
        onPause={noop}
        onResume={noop}
      />,
    );
    expect(lostHtml).not.toContain("Recreate");

    const rowHtml = renderToStaticMarkup(
      <BranchRow
        branch={{ ...sampleBranch, status: "ready" }}
        now={Date.parse("2026-11-13T10:00:00Z")}
        manage
        can={maintainerSession.can}
        mode="page"
        href="/branch/alice-feature-x"
        onOpen={noop}
        onChange={noop}
      />,
    );
    for (const hidden of ["Copy URL", "Pause", "Extend +4h", "Reset", "Delete"]) {
      expect(rowHtml).not.toContain(hidden);
    }

    const branchesHtml = renderToStaticMarkup(<Branches />);
    expect(branchesHtml).not.toContain("> Branch<");

    // A read-only user without base.create does not see the + Base button.
    await setSession({ ...maintainerSession, can: { ...maintainerSession.can, "base.create": false } });
    const basesHtml = renderToStaticMarkup(<Bases />);
    expect(basesHtml).not.toContain("> Base<");
  });
});
