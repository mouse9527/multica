// @vitest-environment node
import { describe, expect, it } from "vitest";
import { checkNewPassword, passwordCharCount } from "./rules";

describe("checkNewPassword", () => {
  it("accepts 12 characters that match", () => {
    expect(checkNewPassword("abcdefghijkl", "abcdefghijkl")).toBeNull();
  });

  it("rejects fewer than 12 characters", () => {
    expect(checkNewPassword("abcdefghijk", "abcdefghijk")).toBe("too_short");
  });

  it("counts code points like the server, not UTF-16 units", () => {
    // Six emoji are twelve UTF-16 units but only six characters.
    const emoji = "😀😀😀😀😀😀";
    expect(emoji.length).toBe(12);
    expect(passwordCharCount(emoji)).toBe(6);
    expect(checkNewPassword(emoji, emoji)).toBe("too_short");
    const twelve = "密码密码密码密码密码密码";
    expect(checkNewPassword(twelve, twelve)).toBeNull();
  });

  it("rejects more than 1024 bytes", () => {
    const long = "é".repeat(513); // 1026 bytes
    expect(checkNewPassword(long, long)).toBe("too_long");
    const max = "a".repeat(1024);
    expect(checkNewPassword(max, max)).toBeNull();
  });

  it("rejects a confirmation that differs", () => {
    expect(checkNewPassword("abcdefghijkl", "abcdefghijkm")).toBe("mismatch");
  });
});
