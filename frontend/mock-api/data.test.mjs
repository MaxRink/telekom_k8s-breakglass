import test from "node:test";
import assert from "node:assert/strict";
import { listSessions } from "./data.mjs";

test("bounds mock scale allocation", () => {
  assert.equal(listSessions({ mockScale: "999999999" }).length, 1000);
});
