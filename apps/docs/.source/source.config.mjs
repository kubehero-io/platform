// source.config.ts
import { defineConfig, defineDocs } from "fumadocs-mdx/config";

// lib/shiki-langs.ts
var shared = {
  string: {
    patterns: [
      {
        name: "string.quoted.double",
        begin: '"',
        end: '"',
        patterns: [{ name: "constant.character.escape", match: "\\\\." }]
      },
      { name: "string.quoted.other", begin: "`", end: "`" }
    ]
  },
  comment: { name: "comment.line.number-sign", match: "#.*$" },
  duration: {
    name: "constant.numeric.duration",
    match: "\\b[0-9]+(?:\\.[0-9]+)?(?:ms|s|m|h|d|w|y)\\b"
  },
  number: { name: "constant.numeric", match: "\\b[0-9]+(?:\\.[0-9]+)?(?:e[+-]?[0-9]+)?\\b" },
  label: {
    name: "entity.name.tag",
    match: "\\b[a-zA-Z_][a-zA-Z0-9_.]*(?=\\s*(?:=~|!~|!=|==|=|>=|<=|>|<))"
  },
  punctuation: { name: "punctuation.definition", match: "[{}()\\[\\],]" }
};
var logqlLang = {
  name: "logql",
  scopeName: "source.logql",
  patterns: [
    { include: "#comment" },
    { include: "#string" },
    { include: "#function" },
    { include: "#keyword" },
    { include: "#label" },
    { include: "#operator" },
    { include: "#duration" },
    { include: "#number" },
    { include: "#punctuation" }
  ],
  repository: {
    ...shared,
    function: {
      name: "support.function",
      match: "\\b(?:count_over_time|rate|rate_counter|bytes_over_time|bytes_rate|absent_over_time|sum_over_time|avg_over_time|max_over_time|min_over_time|quantile_over_time|sum|avg|min|max|count|stddev|stdvar|topk|bottomk|sort|sort_desc)\\b"
    },
    keyword: {
      name: "keyword.control",
      match: "\\b(?:by|without|on|ignoring|json|logfmt|regexp|pattern|unpack|line_format|label_format|drop|keep|unwrap|and|or|unless|bool)\\b"
    },
    operator: { name: "keyword.operator", match: "\\|=|\\|~|!=|!~|=~|>=|<=|==|\\||=|>|<|\\+|-|\\*|/|%|\\^" }
  }
};
var promqlLang = {
  name: "promql",
  scopeName: "source.promql",
  patterns: [
    { include: "#comment" },
    { include: "#string" },
    { include: "#function" },
    { include: "#keyword" },
    { include: "#label" },
    { include: "#operator" },
    { include: "#duration" },
    { include: "#number" },
    { include: "#punctuation" }
  ],
  repository: {
    ...shared,
    function: {
      name: "support.function",
      match: "\\b(?:sum|avg|min|max|count|count_values|stddev|stdvar|topk|bottomk|quantile|group|rate|irate|increase|delta|idelta|deriv|predict_linear|histogram_quantile|abs|ceil|floor|round|clamp_min|clamp_max|time|timestamp|vector|scalar|absent|absent_over_time|avg_over_time|min_over_time|max_over_time|sum_over_time|count_over_time|quantile_over_time|last_over_time|label_replace|label_join|sort|sort_desc)\\b"
    },
    keyword: {
      name: "keyword.control",
      match: "\\b(?:by|without|on|ignoring|group_left|group_right|offset|bool|and|or|unless)\\b"
    },
    operator: { name: "keyword.operator", match: "=~|!~|!=|==|>=|<=|=|>|<|\\+|-|\\*|/|%|\\^" }
  }
};

// lib/shiki-theme.ts
var kubeheroShikiTheme = {
  name: "kubehero-dark",
  type: "dark",
  colors: {
    "editor.background": "#070707",
    "editor.foreground": "#ededeb"
  },
  settings: [
    { settings: { foreground: "#ededeb", background: "#070707" } },
    {
      scope: ["comment", "punctuation.definition.comment"],
      settings: { foreground: "#85857f", fontStyle: "italic" }
    },
    {
      scope: [
        "string",
        "string.quoted",
        "string.unquoted.plain.out.yaml",
        "string.unquoted.plain.in.yaml",
        "string.template"
      ],
      settings: { foreground: "#c9e7a8" }
    },
    {
      scope: [
        "constant.numeric",
        "constant.language",
        "constant.language.boolean",
        "constant.language.null",
        "constant.character.escape"
      ],
      settings: { foreground: "#f5c542" }
    },
    {
      scope: [
        "entity.name.tag",
        "entity.name.tag.yaml",
        "support.type.property-name",
        "support.type.property-name.json",
        "meta.object-literal.key",
        "variable.other.property"
      ],
      settings: { foreground: "#6dd3ff" }
    },
    {
      scope: ["keyword", "storage", "storage.type", "keyword.control"],
      settings: { foreground: "#6dd3ff" }
    },
    {
      scope: [
        "entity.name.function",
        "support.function",
        "support.function.builtin",
        "entity.name.command"
      ],
      settings: { foreground: "#3ee089" }
    },
    {
      scope: [
        "variable.other.normal.shell",
        "variable.other.special.shell",
        "variable.other.positional.shell",
        "punctuation.definition.variable.shell"
      ],
      settings: { foreground: "#f5c542" }
    },
    {
      scope: ["constant.other.option", "variable.parameter"],
      settings: { foreground: "#b5b5b0" }
    },
    {
      scope: [
        "punctuation",
        "meta.brace",
        "keyword.operator",
        "punctuation.separator",
        "punctuation.definition"
      ],
      settings: { foreground: "#8a8a85" }
    },
    { scope: ["entity.name.type", "support.type"], settings: { foreground: "#3ee089" } },
    { scope: ["markup.inserted"], settings: { foreground: "#3ee089" } },
    { scope: ["markup.deleted"], settings: { foreground: "#ff6a45" } }
  ]
};

// source.config.ts
var docs = defineDocs({ dir: "content/docs" });
var source_config_default = defineConfig({
  mdxOptions: {
    rehypeCodeOptions: {
      themes: { light: kubeheroShikiTheme, dark: kubeheroShikiTheme },
      // Shiki ships no LogQL / PromQL grammar — register our own.
      langs: [logqlLang, promqlLang],
      fallbackLanguage: "plaintext"
    }
  }
});
export {
  source_config_default as default,
  docs
};
