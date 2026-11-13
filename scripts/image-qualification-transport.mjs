// Explicit v2 profile: requested props remain in the intent coordinate. Only the
// observed, known provider representation may omit them. Legacy shapes stay strict.
import { canonical, digest } from "./image-qualification-isolation.mjs";
const profileName = "QUALIFICATION_TRANSPORT_PROFILE";
const identityName = "QUALIFICATION_TRANSPORT_IDENTITY";
export function transportProfile(bindings) {
  const markers = bindings.filter((b) => b.name === profileName);
  if (!markers.length) return undefined;
  const identities = bindings.filter((b) => b.name === identityName);
  if (
    markers.length !== 1 ||
    identities.length !== 1 ||
    markers[0].type !== "plain_text" ||
    !["stage-v2", "final-v2", "controller-v2", "relay-v2"].includes(markers[0].text) ||
    identities[0].type !== "plain_text" ||
    typeof identities[0].text !== "string" ||
    identities[0].text.length > 2048
  )
    throw Error("invalid transport ownership profile");
  const identity = JSON.parse(identities[0].text);
  if (
    !identity ||
    typeof identity !== "object" ||
    Array.isArray(identity) ||
    !/^[a-f0-9]{64}$/.test(identity.deploymentHash ?? "")
  )
    throw Error("invalid transport intent");
  return { profile: markers[0].text, identity };
}
export function transportMarkers(profile, identity) {
  return [
    { name: profileName, type: "plain_text", text: profile },
    { name: identityName, type: "plain_text", text: canonical(identity) },
  ];
}
export function transportBindingShape(bindings) {
  const profile = transportProfile(bindings);
  if (!profile) return bindings;
  return bindings.map((b) => {
    if (b.type !== "service") return b;
    const expected =
      profile.profile === "final-v2"
        ? {
            name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
            service: "crabbox-aws-qualification-authority",
            props: profile.identity,
          }
        : profile.profile === "controller-v2"
          ? {
              name: "AUTHORITY",
              service: "crabbox-aws-qualification-authority",
              entrypoint: "AWSQualificationController",
              props: { deploymentHash: profile.identity.deploymentHash },
            }
          : profile.profile === "relay-v2"
            ? { name: "CANDIDATE", service: profile.identity.candidateWorker }
            : undefined;
    if (
      !expected ||
      b.name !== expected.name ||
      b.service !== expected.service ||
      b.entrypoint !== expected.entrypoint ||
      (b.environment !== undefined && b.environment !== "production") ||
      (Object.hasOwn(b, "props") && canonical(b.props) !== canonical(expected.props))
    )
      throw Error("transport capability differs from intent");
    // Keep every unknown coordinate: ownership hashes must detect extra capability.
    const { props, environment, ...visible } = b;
    return visible;
  });
}

function exactReceipt(receipt, request, state) {
  const allowed = [
    "protocol",
    "state",
    ...Object.keys(request),
    ...(receipt && Object.hasOwn(receipt, "observedAt") ? ["observedAt"] : []),
  ].sort();
  if (
    !receipt ||
    canonical(Object.keys(receipt).sort()) !== canonical(allowed) ||
    receipt.protocol !== 1 ||
    receipt.state !== state ||
    Object.keys(request).some((key) => canonical(receipt[key]) !== canonical(request[key])) ||
    ((state === "observed" || state === "verified" || Object.hasOwn(receipt, "observedAt")) &&
      (!Number.isFinite(Date.parse(receipt.observedAt)) ||
        Date.parse(receipt.observedAt) >= Date.parse(request.expiresAt)))
  )
    throw Error("canonical transport receipt mismatch");
  return receipt;
}

// The existing protected controller/registry own effects; local custody records
// exact intent before open. Recovery never redispatches a candidate or opens anew.
export async function proveCandidateTransport({
  request,
  controller,
  invokeCandidate,
  verifyFences,
  save,
  signal,
}) {
  const check = () => {
    if (signal?.aborted || Date.now() >= Date.parse(request.expiresAt))
      throw Error("transport proof cancelled or expired");
  };
  await save({ request, phase: "opening" });
  check();
  await verifyFences();
  exactReceipt(await controller("transport-open", request), request, "pending");
  await save({ request, phase: "dispatching" });
  check();
  // Status, body, headers and self-reported identity from candidate are not proof.
  await invokeCandidate(request.challenge);
  check();
  const reply = await controller("transport-discover", {});
  exactReceipt(reply.probe, request, "observed");
  await verifyFences();
  check();
  const settled = exactReceipt(
    await controller("transport-settle", { challenge: request.challenge, verified: true }),
    request,
    "verified",
  );
  const after = await controller("transport-discover", {});
  exactReceipt(after.probe, request, "verified");
  await save({ request, phase: "verified", receiptSha256: digest(canonical(settled)) });
  return settled;
}

export async function settleCandidateTransport({ request, controller, save }) {
  const reply = await controller("transport-discover", {});
  if (reply.probe === null) {
    // A missing slot after uncertain open is not proof of a completed attempt.
    throw Error("transport probe intent is unobserved; retain custody");
  }
  const state = reply.probe?.state;
  if (!["pending", "observed", "verified", "cancelled"].includes(state))
    throw Error("unknown transport probe state");
  exactReceipt(reply.probe, request, state);
  const settled = await controller("transport-settle", {
    challenge: request.challenge,
    verified: state === "verified",
  });
  exactReceipt(settled, request, state === "verified" ? "verified" : "cancelled");
  const after = await controller("transport-discover", {});
  if (canonical(after.probe) !== canonical(settled))
    throw Error("transport settlement unconfirmed");
  await save({ request, phase: settled.state, receiptSha256: digest(canonical(settled)) });
  return settled;
}
