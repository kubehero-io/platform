// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

import { RouteError } from "@/components/ui/route-error";

export default function ProfilesError(props: { error: Error & { digest?: string }; reset: () => void }) {
  return <RouteError view="profiles" {...props} />;
}
