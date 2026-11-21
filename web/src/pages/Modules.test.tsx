import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { ModuleState } from "../api";
import { ForgetModuleDialog, ModuleRow } from "./Modules";

const noop = () => {};

const mod = (over: Partial<ModuleState> = {}): ModuleState => ({
  name: "audit",
  version: "0.2.0",
  wanted: false,
  nodes: [{ node: "node1", loaded: "", current: false }],
  requires_license: false,
  ...over,
});

describe("ModuleRow", () => {
  it("削除済みのモジュール行にだけ Forget ボタンを Enable と並べて表示する", () => {
    const removed = renderToStaticMarkup(
      <ModuleRow name="audit" description="Audit trail" state={mod({ wanted: false })} licensed={false} trial={false} onEnable={noop} onDisable={noop} onForget={noop} />,
    );
    expect(removed).toContain(">Enable</button>");
    expect(removed).toContain(">Forget</button>");

    const enabled = renderToStaticMarkup(
      <ModuleRow name="audit" description="Audit trail" state={mod({ wanted: true })} licensed={false} trial={false} onEnable={noop} onDisable={noop} onForget={noop} />,
    );
    expect(enabled).toContain(">Disable</button>");
    expect(enabled).not.toContain(">Forget</button>");

    const unrecorded = renderToStaticMarkup(
      <ModuleRow name="audit" description="Audit trail" state={undefined} licensed={false} trial={false} onEnable={noop} onDisable={noop} onForget={noop} />,
    );
    expect(unrecorded).toContain(">Enable</button>");
    expect(unrecorded).not.toContain(">Forget</button>");
  });
});

describe("ModuleRow の Paid バッジ", () => {
  const render = (state: ModuleState | undefined, over: { licensed?: boolean; trial?: boolean; licenseAll?: boolean } = {}) =>
    renderToStaticMarkup(
      <ModuleRow name="m" state={state} licensed={over.licensed ?? false} trial={over.trial ?? false} licenseAll={over.licenseAll} onEnable={noop} onDisable={noop} />,
    );

  it("ライセンスが全モジュールを含んでいても、有償でないモジュールには Paid バッジを付けない", () => {
    const html = render(mod({ wanted: true, requires_license: false }), { licensed: true, licenseAll: true });
    expect(html).not.toContain("paid-badge");
  });

  it("ライセンスが全モジュールを含んでいても、削除済みで有償でない記録には Paid バッジを付けない", () => {
    const html = render(mod({ wanted: false, requires_license: false }), { licensed: true, licenseAll: true });
    expect(html).not.toContain("paid-badge");
  });

  it("有償のモジュールには、ライセンスが全モジュールを含んでいても Paid · licensed を付ける", () => {
    const html = render(mod({ wanted: true, requires_license: true }), { licensed: true, licenseAll: true });
    expect(html).toContain("Paid · licensed");
  });

  it("ライセンスがモジュール名を列挙しているときは、まだ入れていないモジュールにも Paid · licensed を付ける", () => {
    const html = render(undefined, { licensed: true, licenseAll: false });
    expect(html).toContain("Paid · licensed");
  });

  it("試用中は、有償と分かっているモジュールにだけ Paid · trial を付ける", () => {
    expect(render(mod({ wanted: true, requires_license: true }), { licensed: true, trial: true, licenseAll: true })).toContain("Paid · trial");
    expect(render(mod({ wanted: true, requires_license: false }), { licensed: true, trial: true, licenseAll: true })).not.toContain("paid-badge");
  });
});

describe("ForgetModuleDialog", () => {
  it("モジュール名と確認メッセージと操作ボタンを表示する", () => {
    const html = renderToStaticMarkup(<ForgetModuleDialog name="audit" onCancel={noop} onConfirm={noop} />);
    expect(html).toContain("Forget audit?");
    expect(html).toContain("Remove the cluster record of this module so it is no longer listed.");
    expect(html).toContain(">Cancel</button>");
    expect(html).toContain(">Forget</button>");
  });
});
