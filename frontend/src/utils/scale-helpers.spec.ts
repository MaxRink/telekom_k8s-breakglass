import { describe, it, expect } from "vitest";
import { extractScaleValue } from "./scale-helpers";

describe("scale-helpers", () => {
  describe("extractScaleValue", () => {
    it("extracts value from standard target.value", () => {
      const input = document.createElement("input");
      input.value = "test-value";
      const event = new Event("input");
      Object.defineProperty(event, "target", { value: input });

      expect(extractScaleValue(event)).toBe("test-value");
    });

    it("extracts value from CustomEvent detail.value", () => {
      const event = new CustomEvent("scale-change", {
        detail: { value: "custom-value" },
      });

      expect(extractScaleValue(event)).toBe("custom-value");
    });

    it("returns empty string if neither target nor detail has a string value", () => {
      const event = new Event("click");
      expect(extractScaleValue(event)).toBe("");
    });
  });
});
