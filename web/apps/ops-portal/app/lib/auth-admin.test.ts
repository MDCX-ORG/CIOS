/**
 * Pure helpers for Platform Admin gate (L109 P801).
 * node --experimental-strip-types --test app/lib/auth-admin.test.ts
 */
import assert from "node:assert/strict";
import { describe, it } from "node:test";

// Node ESM + --experimental-strip-types requires the .ts suffix; excluded from tsc.
import { rolesFromJwtPayload, sessionIsAdmin } from "./auth-admin.ts";

describe("rolesFromJwtPayload", () => {
  it("reads scope array", () => {
    assert.deepEqual(rolesFromJwtPayload({ scope: ["viewer", "admin"] }), [
      "viewer",
      "admin",
    ]);
  });
  it("reads space-separated scope string", () => {
    assert.deepEqual(rolesFromJwtPayload({ scope: "operator viewer" }), [
      "operator",
      "viewer",
    ]);
  });
  it("merges roles claim", () => {
    assert.deepEqual(
      rolesFromJwtPayload({ scope: ["viewer"], roles: ["admin"] }),
      ["viewer", "admin"],
    );
  });
});

describe("sessionIsAdmin", () => {
  it("true for admin role", () => {
    assert.equal(
      sessionIsAdmin({ user: { sub: "u1", roles: ["operator", "admin"] } }),
      true,
    );
  });
  it("true for lab admin subject", () => {
    assert.equal(
      sessionIsAdmin({ user: { sub: "svc:lab-admin", roles: ["viewer"] } }),
      true,
    );
  });
  it("true for dev bypass subjects", () => {
    assert.equal(
      sessionIsAdmin({ user: { sub: "dev-no-auth", roles: [] } }),
      true,
    );
    assert.equal(sessionIsAdmin({ user: { sub: "dev", roles: [] } }), true);
  });
  it("false for operator-only", () => {
    assert.equal(
      sessionIsAdmin({
        user: { sub: "svc:lab-operator", roles: ["operator"] },
      }),
      false,
    );
  });
});
