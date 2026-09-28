// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
//
// Mints a kh_session cookie value the way lib/session-crypto.ts does, for
// smoke tests against a running dashboard. Needs the server's secret:
//
//   KUBEHERO_SESSION_SECRET=… node scripts/mint-session.mjs ['{"email":…}']
//
// Default session: a signed-in, onboarded demo admin.

import { createCipheriv, createHmac, hkdfSync, randomBytes } from "node:crypto";

const secret = process.env.KUBEHERO_SESSION_SECRET?.trim() || "kubehero-insecure-dev-secret-set-KUBEHERO_SESSION_SECRET";
const salt = Buffer.from("kubehero.session.v2");
const enc = Buffer.from(hkdfSync("sha256", Buffer.from(secret), salt, "aes-256-gcm", 32));
const mac = Buffer.from(hkdfSync("sha256", Buffer.from(secret), salt, "hmac-sha256", 32));
const session = JSON.parse(
  process.argv[2] || '{"email":"dev@acme.io","org":"acme","onboarded":true,"createdAt":1,"mode":"demo","role":"admin"}',
);
const iv = randomBytes(12);
const c = createCipheriv("aes-256-gcm", enc, iv);
c.setAAD(Buffer.from("kh_session"));
const ct = Buffer.concat([c.update(JSON.stringify(session), "utf8"), c.final()]);
const payload = Buffer.concat([Buffer.from([2]), iv, ct, c.getAuthTag()]).toString("base64url");
const sig = createHmac("sha256", mac).update(payload).digest("base64url");
process.stdout.write(`${payload}.${sig}`);
