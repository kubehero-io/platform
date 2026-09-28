import type { ThemeRegistrationRaw } from "shiki";

/* Brand syntax theme shared by the site's code blocks and the docs.
   Palette-locked: keys/keywords in cool cyan, strings in a soft signal
   green, numbers/booleans in amber, punctuation dim, comments faint. The
   accent never appears here — code isn't urgent. */
export const kubeheroShikiTheme: ThemeRegistrationRaw = {
  name: "kubehero-dark",
  type: "dark",
  colors: {
    "editor.background": "#070707",
    "editor.foreground": "#ededeb",
  },
  settings: [
    { settings: { foreground: "#ededeb", background: "#070707" } },
    {
      scope: ["comment", "punctuation.definition.comment"],
      settings: { foreground: "#85857f", fontStyle: "italic" },
    },
    {
      scope: [
        "string",
        "string.quoted",
        "string.unquoted.plain.out.yaml",
        "string.unquoted.plain.in.yaml",
        "string.template",
      ],
      settings: { foreground: "#c9e7a8" },
    },
    {
      scope: [
        "constant.numeric",
        "constant.language",
        "constant.language.boolean",
        "constant.language.null",
        "constant.character.escape",
      ],
      settings: { foreground: "#f5c542" },
    },
    {
      scope: [
        "entity.name.tag",
        "entity.name.tag.yaml",
        "support.type.property-name",
        "support.type.property-name.json",
        "meta.object-literal.key",
        "variable.other.property",
      ],
      settings: { foreground: "#6dd3ff" },
    },
    {
      scope: ["keyword", "storage", "storage.type", "keyword.control"],
      settings: { foreground: "#6dd3ff" },
    },
    {
      scope: [
        "entity.name.function",
        "support.function",
        "support.function.builtin",
        "entity.name.command",
      ],
      settings: { foreground: "#3ee089" },
    },
    {
      scope: [
        "variable.other.normal.shell",
        "variable.other.special.shell",
        "variable.other.positional.shell",
        "punctuation.definition.variable.shell",
      ],
      settings: { foreground: "#f5c542" },
    },
    {
      scope: ["constant.other.option", "variable.parameter"],
      settings: { foreground: "#b5b5b0" },
    },
    {
      scope: [
        "punctuation",
        "meta.brace",
        "keyword.operator",
        "punctuation.separator",
        "punctuation.definition",
      ],
      settings: { foreground: "#8a8a85" },
    },
    { scope: ["entity.name.type", "support.type"], settings: { foreground: "#3ee089" } },
    { scope: ["markup.inserted"], settings: { foreground: "#3ee089" } },
    { scope: ["markup.deleted"], settings: { foreground: "#ff6a45" } },
  ],
};
