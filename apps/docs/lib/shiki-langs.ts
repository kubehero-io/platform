import type { LanguageRegistration } from "shiki";

/* Minimal TextMate grammars for the query languages the docs use and Shiki
   doesn't ship. Scopes map onto the brand theme (lib/shiki-theme.ts):
   labels → entity.name.tag, strings, durations/numbers → constant.numeric,
   functions → support.function, keywords/parsers → keyword. */

const shared = {
  string: {
    patterns: [
      {
        name: "string.quoted.double",
        begin: '"',
        end: '"',
        patterns: [{ name: "constant.character.escape", match: "\\\\." }],
      },
      { name: "string.quoted.other", begin: "`", end: "`" },
    ],
  },
  comment: { name: "comment.line.number-sign", match: "#.*$" },
  duration: {
    name: "constant.numeric.duration",
    match: "\\b[0-9]+(?:\\.[0-9]+)?(?:ms|s|m|h|d|w|y)\\b",
  },
  number: { name: "constant.numeric", match: "\\b[0-9]+(?:\\.[0-9]+)?(?:e[+-]?[0-9]+)?\\b" },
  label: {
    name: "entity.name.tag",
    match: "\\b[a-zA-Z_][a-zA-Z0-9_.]*(?=\\s*(?:=~|!~|!=|==|=|>=|<=|>|<))",
  },
  punctuation: { name: "punctuation.definition", match: "[{}()\\[\\],]" },
};

export const logqlLang: LanguageRegistration = {
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
    { include: "#punctuation" },
  ],
  repository: {
    ...shared,
    function: {
      name: "support.function",
      match:
        "\\b(?:count_over_time|rate|rate_counter|bytes_over_time|bytes_rate|absent_over_time|sum_over_time|avg_over_time|max_over_time|min_over_time|quantile_over_time|sum|avg|min|max|count|stddev|stdvar|topk|bottomk|sort|sort_desc)\\b",
    },
    keyword: {
      name: "keyword.control",
      match: "\\b(?:by|without|on|ignoring|json|logfmt|regexp|pattern|unpack|line_format|label_format|drop|keep|unwrap|and|or|unless|bool)\\b",
    },
    operator: { name: "keyword.operator", match: "\\|=|\\|~|!=|!~|=~|>=|<=|==|\\||=|>|<|\\+|-|\\*|/|%|\\^" },
  },
};

export const promqlLang: LanguageRegistration = {
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
    { include: "#punctuation" },
  ],
  repository: {
    ...shared,
    function: {
      name: "support.function",
      match:
        "\\b(?:sum|avg|min|max|count|count_values|stddev|stdvar|topk|bottomk|quantile|group|rate|irate|increase|delta|idelta|deriv|predict_linear|histogram_quantile|abs|ceil|floor|round|clamp_min|clamp_max|time|timestamp|vector|scalar|absent|absent_over_time|avg_over_time|min_over_time|max_over_time|sum_over_time|count_over_time|quantile_over_time|last_over_time|label_replace|label_join|sort|sort_desc)\\b",
    },
    keyword: {
      name: "keyword.control",
      match: "\\b(?:by|without|on|ignoring|group_left|group_right|offset|bool|and|or|unless)\\b",
    },
    operator: { name: "keyword.operator", match: "=~|!~|!=|==|>=|<=|=|>|<|\\+|-|\\*|/|%|\\^" },
  },
};
