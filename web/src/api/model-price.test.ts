import test from "node:test";
import assert from "node:assert/strict";
import {
  parsePriceInput,
  parseEditPrices,
  initialEditValues,
  shouldShowRestoreAuto,
  canInteract,
} from "./model-price.ts";

void test("parsePriceInput: 空与空白拒", () => {
  assert.equal(parsePriceInput(""), null);
  assert.equal(parsePriceInput("   "), null);
});

void test("parsePriceInput: 非数字拒", () => {
  assert.equal(parsePriceInput("abc"), null);
  assert.equal(parsePriceInput("1.2.3"), null);
});

void test("parsePriceInput: 负数拒", () => {
  assert.equal(parsePriceInput("-1"), null);
  assert.equal(parsePriceInput("-0.01"), null);
});

void test("parsePriceInput: Infinity 与 NaN 拒", () => {
  assert.equal(parsePriceInput("Infinity"), null);
  assert.equal(parsePriceInput("-Infinity"), null);
  assert.equal(parsePriceInput("NaN"), null);
});

void test("parsePriceInput: 显式 0 有效", () => {
  assert.equal(parsePriceInput("0"), 0);
  assert.equal(parsePriceInput("0.00"), 0);
  assert.equal(parsePriceInput("  0  "), 0);
});

void test("parsePriceInput: 正数有效(含小数)", () => {
  assert.equal(parsePriceInput("0.5"), 0.5);
  assert.equal(parsePriceInput("1.25"), 1.25);
  assert.equal(parsePriceInput(" 2 "), 2);
});

void test("parseEditPrices: 四项全有效才通过", () => {
  const ok = parseEditPrices({
    input: "1",
    output: "2",
    cache_read: "0.5",
    cache_write: "0",
  });
  assert.equal(ok?.input, 1);
  assert.equal(ok?.output, 2);
  assert.equal(ok?.cache_read, 0.5);
  assert.equal(ok?.cache_write, 0);
});

void test("parseEditPrices: 任一项空/非法整体拒", () => {
  const base = { input: "0", output: "0", cache_read: "0", cache_write: "0" };
  assert.equal(parseEditPrices({ ...base, input: "" }), null);
  assert.equal(parseEditPrices({ ...base, output: "  " }), null);
  assert.equal(parseEditPrices({ ...base, cache_read: "-0.1" }), null);
  assert.equal(parseEditPrices({ ...base, cache_write: "x" }), null);
});

void test("initialEditValues: 已知时回显价", () => {
  const v = initialEditValues(true, {
    input: 0,
    output: 0,
    cache_read: 0,
    cache_write: 0,
  });
  assert.deepEqual(v, {
    input: "0",
    output: "0",
    cache_read: "0",
    cache_write: "0",
  });
});

void test("initialEditValues: 已知非零仍回显", () => {
  const v = initialEditValues(true, {
    input: 0.5,
    output: 1.2,
    cache_read: 0.1,
    cache_write: 0.2,
  });
  assert.equal(v.input, "0.5");
  assert.equal(v.output, "1.2");
});

void test("initialEditValues: price_known=false 全部空", () => {
  const v = initialEditValues(false, {
    input: 1,
    output: 2,
    cache_read: 3,
    cache_write: 4,
  });
  assert.deepEqual(v, {
    input: "",
    output: "",
    cache_read: "",
    cache_write: "",
  });
});

void test("initialEditValues: price=null 全部空(契约未知)", () => {
  const v = initialEditValues(true, null);
  assert.deepEqual(v, {
    input: "",
    output: "",
    cache_read: "",
    cache_write: "",
  });
});

void test("shouldShowRestoreAuto: 仅 manual 显示", () => {
  assert.equal(shouldShowRestoreAuto("manual"), true);
  assert.equal(shouldShowRestoreAuto("auto"), false);
});

void test("canInteract: 任一 pending 拒", () => {
  assert.equal(canInteract(false, false), true);
  assert.equal(canInteract(true, false), false);
  assert.equal(canInteract(false, true), false);
  assert.equal(canInteract(true, true), false);
});
