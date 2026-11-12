import { describe, expect, it } from "vitest";
import { billingLabel, renewalInfo, resultLabel } from "./license";

describe("billingLabel", () => {
  it("請求の種類を表し手動のキーは空にする", () => {
    expect(billingLabel("monthly")).toBe("Monthly");
    expect(billingLabel("yearly")).toBe("Yearly");
    expect(billingLabel("")).toBe("");
    expect(billingLabel(undefined)).toBe("");
    expect(billingLabel("weekly")).toBe("");
  });
});

describe("resultLabel", () => {
  it("結果ごとの文言を返す", () => {
    expect(resultLabel("renewed")).toBe("Renewed");
    expect(resultLabel("not_yet")).toBe("No newer key yet");
    expect(resultLabel("inactive")).toBe("Subscription not active");
    expect(resultLabel("error")).toBe("Failed");
    expect(resultLabel("unknown")).toBe("Unknown");
    expect(resultLabel(undefined)).toBe("");
    expect(resultLabel("")).toBe("");
  });
});

describe("renewalInfo", () => {
  it("サブスクリプションのない鍵は自動更新の対象にならない", () => {
    const i = renewalInfo({}, { enabled: true }, true);
    expect(i).toMatchObject({ subscribed: false, auto: false, canCheck: false, warn: false });
  });
  it("自動更新が入っていれば次の確認と今すぐ確認を出す", () => {
    const i = renewalInfo({ subscription: "sub_1" }, { enabled: true, next_attempt: "2026-11-11T00:00:00Z", last_attempt: "2026-11-10T00:00:00Z", last_result: "not_yet" }, true);
    expect(i).toMatchObject({ subscribed: true, auto: true, canCheck: true, next: "2026-11-11T00:00:00Z", last: "2026-11-10T00:00:00Z", result: "not_yet", warn: false });
  });
  it("状態がまだ届いていなくても既定は自動更新", () => {
    expect(renewalInfo({ subscription: "sub_1" }, undefined, true)).toMatchObject({ auto: true, canCheck: true, next: undefined });
  });
  it("自動更新を切ると確認ボタンも次の確認も出さない", () => {
    const i = renewalInfo({ subscription: "sub_1" }, { enabled: false, next_attempt: "2026-11-11T00:00:00Z" }, true);
    expect(i).toMatchObject({ subscribed: true, auto: false, canCheck: false, next: undefined });
  });
  it("変更の権限がなければ確認ボタンを出さない", () => {
    expect(renewalInfo({ subscription: "sub_1" }, { enabled: true }, false).canCheck).toBe(false);
  });
  it("失敗や解約は警告にする", () => {
    for (const r of ["error", "inactive"] as const) {
      expect(renewalInfo({ subscription: "s" }, { enabled: true, last_result: r, message: "x" }, true)).toMatchObject({ warn: true, message: "x" });
    }
    expect(renewalInfo({ subscription: "s" }, { enabled: true, last_result: "renewed" }, true).warn).toBe(false);
  });
});
