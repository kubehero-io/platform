// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { redirect } from "next/navigation";

export default function Home() {
  redirect("/overview");
}
