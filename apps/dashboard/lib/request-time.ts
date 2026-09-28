// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
import "server-only";

// One "now" per server request. Server components that derive windows
// from the current time (last 24h, "5m ago") all read the same instant,
// so panels on one page never disagree by the few milliseconds between
// their renders — and render stays a pure function of the request.

import { cache } from "react";

export const requestNow: () => number = cache(() => Date.now());
