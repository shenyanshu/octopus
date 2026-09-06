import test from "node:test";
import assert from "node:assert/strict";
import { formatLogCost } from "./log-cost.ts";

// t mock: key -> label
const t = (key: string, values?: Record<string, string>) => {
  const map: Record<string, string> = {
    costPending: "-",
    costUnknown: "未计价",
    estimatedByGroup: `按分组名参考价估算(${values?.model ?? "?"})`,
  };
  return map[key] ?? key;
};

void test("running 状态 -> costPending(-)", () => {
  const r = formatLogCost(
    {
      cost: null,
      cost_known: false,
      cost_source: "actual_reference",
      cost_reference_model: null,
    },
    t,
  );
  assert.equal(r.value, "-");
  assert.equal(r.tooltip, undefined);
});

void test("terminal unknown -> 未计价, 不是 0", () => {
  const r = formatLogCost(
    {
      cost: null,
      cost_known: false,
      cost_source: "unknown",
      cost_reference_model: null,
    },
    t,
  );
  assert.equal(r.value, "未计价");
  assert.equal(r.tooltip, undefined);
});

void test("known cost=0 manual -> 精确 0.000000, 不为免费", () => {
  const r = formatLogCost(
    {
      cost: 0,
      cost_known: true,
      cost_source: "manual",
      cost_reference_model: null,
    },
    t,
  );
  assert.equal(r.value, "0.000000");
  assert.equal(r.tooltip, undefined);
});

void test("group_reference -> ≈前缀 + 参考模型 tooltip", () => {
  const r = formatLogCost(
    {
      cost: 0.000012,
      cost_known: true,
      cost_source: "group_reference",
      cost_reference_model: "gpt-4o-mini",
    },
    t,
  );
  assert.equal(r.value, "≈0.000012");
  assert.equal(r.tooltip, "按分组名参考价估算(gpt-4o-mini)");
});

void test("actual_reference -> 精确数字, 无 ≈, 无 tooltip", () => {
  const r = formatLogCost(
    {
      cost: 0.000456,
      cost_known: true,
      cost_source: "actual_reference",
      cost_reference_model: null,
    },
    t,
  );
  assert.equal(r.value, "0.000456");
  assert.equal(r.tooltip, undefined);
});
