// @ts-nocheck
import * as __fd_glob_29 from "../content/docs/troubleshooting.mdx?collection=docs"
import * as __fd_glob_28 from "../content/docs/stack.mdx?collection=docs"
import * as __fd_glob_27 from "../content/docs/security.mdx?collection=docs"
import * as __fd_glob_26 from "../content/docs/rightsizing.mdx?collection=docs"
import * as __fd_glob_25 from "../content/docs/quickstart.mdx?collection=docs"
import * as __fd_glob_24 from "../content/docs/profiling.mdx?collection=docs"
import * as __fd_glob_23 from "../content/docs/production.mdx?collection=docs"
import * as __fd_glob_22 from "../content/docs/overview.mdx?collection=docs"
import * as __fd_glob_21 from "../content/docs/network.mdx?collection=docs"
import * as __fd_glob_20 from "../content/docs/metrics-reference.mdx?collection=docs"
import * as __fd_glob_19 from "../content/docs/logs.mdx?collection=docs"
import * as __fd_glob_18 from "../content/docs/integrations/prometheus-grafana.mdx?collection=docs"
import * as __fd_glob_17 from "../content/docs/integrations/identity.mdx?collection=docs"
import * as __fd_glob_16 from "../content/docs/integrations/clouds.mdx?collection=docs"
import * as __fd_glob_15 from "../content/docs/index.mdx?collection=docs"
import * as __fd_glob_14 from "../content/docs/faq.mdx?collection=docs"
import * as __fd_glob_13 from "../content/docs/deploy.mdx?collection=docs"
import * as __fd_glob_12 from "../content/docs/crd-reference.mdx?collection=docs"
import * as __fd_glob_11 from "../content/docs/concepts.mdx?collection=docs"
import * as __fd_glob_10 from "../content/docs/compatibility.mdx?collection=docs"
import * as __fd_glob_9 from "../content/docs/comparison.mdx?collection=docs"
import * as __fd_glob_8 from "../content/docs/cli.mdx?collection=docs"
import * as __fd_glob_7 from "../content/docs/chargeback.mdx?collection=docs"
import * as __fd_glob_6 from "../content/docs/architecture.mdx?collection=docs"
import * as __fd_glob_5 from "../content/docs/api-reference.mdx?collection=docs"
import * as __fd_glob_4 from "../content/docs/allocation.mdx?collection=docs"
import * as __fd_glob_3 from "../content/docs/alerts.mdx?collection=docs"
import * as __fd_glob_2 from "../content/docs/agents.mdx?collection=docs"
import { default as __fd_glob_1 } from "../content/docs/meta.json?collection=docs"
import { default as __fd_glob_0 } from "../content/docs/integrations/meta.json?collection=docs"
import { server } from 'fumadocs-mdx/runtime/server';
import type * as Config from '../source.config';

const create = server<typeof Config, import("fumadocs-mdx/runtime/types").InternalTypeConfig & {
  DocData: {
  }
}>();

export const docs = await create.docs("docs", "content/docs", {"integrations/meta.json": __fd_glob_0, "meta.json": __fd_glob_1, }, {"agents.mdx": __fd_glob_2, "alerts.mdx": __fd_glob_3, "allocation.mdx": __fd_glob_4, "api-reference.mdx": __fd_glob_5, "architecture.mdx": __fd_glob_6, "chargeback.mdx": __fd_glob_7, "cli.mdx": __fd_glob_8, "comparison.mdx": __fd_glob_9, "compatibility.mdx": __fd_glob_10, "concepts.mdx": __fd_glob_11, "crd-reference.mdx": __fd_glob_12, "deploy.mdx": __fd_glob_13, "faq.mdx": __fd_glob_14, "index.mdx": __fd_glob_15, "integrations/clouds.mdx": __fd_glob_16, "integrations/identity.mdx": __fd_glob_17, "integrations/prometheus-grafana.mdx": __fd_glob_18, "logs.mdx": __fd_glob_19, "metrics-reference.mdx": __fd_glob_20, "network.mdx": __fd_glob_21, "overview.mdx": __fd_glob_22, "production.mdx": __fd_glob_23, "profiling.mdx": __fd_glob_24, "quickstart.mdx": __fd_glob_25, "rightsizing.mdx": __fd_glob_26, "security.mdx": __fd_glob_27, "stack.mdx": __fd_glob_28, "troubleshooting.mdx": __fd_glob_29, });