import { defineConfig, defineDocs } from "fumadocs-mdx/config";
import { logqlLang, promqlLang } from "./lib/shiki-langs";
import { kubeheroShikiTheme } from "./lib/shiki-theme";

// Same content and highlighting as the kubehero.io docs — content/docs is
// synced from the site so both render identically.
export const docs = defineDocs({ dir: "content/docs" });

export default defineConfig({
  mdxOptions: {
    rehypeCodeOptions: {
      themes: { light: kubeheroShikiTheme, dark: kubeheroShikiTheme },
      // Shiki ships no LogQL / PromQL grammar — register our own.
      langs: [logqlLang, promqlLang],
      fallbackLanguage: "plaintext",
    },
  },
});
