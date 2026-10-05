import test from "node:test";
import assert from "node:assert/strict";
import { isMessage, messageDeliveryFingerprint } from "./protocol.ts";
import type { CrossMachineProvenance, Message } from "../types.ts";

const origin = { name: "worker", sessionId: "origin-session", machine: "laptop" };
const provenance: CrossMachineProvenance = {
  type: "ssh-relay",
  version: 1,
  trust: "ssh-asserted",
  origin,
};

function messageWith(crossMachine: unknown): unknown {
  return {
    id: "message-1",
    timestamp: 1,
    crossMachine,
    content: { text: "hello" },
  };
}

test("protocol validates the complete SSH relay provenance discriminator and origin", () => {
  assert.equal(isMessage(messageWith(provenance)), true);

  const invalid: unknown[] = [
    { ...provenance, type: "ssh" },
    { ...provenance, type: undefined },
    { ...provenance, version: 2 },
    { ...provenance, version: undefined },
    { ...provenance, trust: "verified" },
    { ...provenance, trust: undefined },
    { ...provenance, origin: undefined },
    { ...provenance, origin: { ...origin, name: 1 } },
    { ...provenance, origin: { ...origin, sessionId: 1 } },
    { ...provenance, origin: { ...origin, machine: 1 } },
  ];
  for (const crossMachine of invalid) {
    assert.equal(isMessage(messageWith(crossMachine)), false, JSON.stringify(crossMachine));
  }

  assert.equal(isMessage({
    ...(messageWith(provenance) as Record<string, unknown>),
    provenance: {
      type: "extension_outbox",
      extensionId: "extension-id",
      extensionName: "Extension",
      requestId: "request-id",
    },
  }), true);
});

test("broker replay fingerprints distinguish every asserted origin field", () => {
  const message = messageWith(provenance) as Message;
  const original = messageDeliveryFingerprint(message, "receiver-session");
  for (const [field, value] of [["name", "other"], ["sessionId", "other"], ["machine", "other"]] as const) {
    const changed: Message = {
      ...message,
      crossMachine: { ...provenance, origin: { ...origin, [field]: value } },
    };
    assert.notEqual(messageDeliveryFingerprint(changed, "receiver-session"), original);
  }
});
