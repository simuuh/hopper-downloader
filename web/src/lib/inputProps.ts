// literalText is for fields that take text verbatim — links, paths, user
// names, tokens, passwords, captcha answers: phone keyboards must not
// capitalize the first letter, "correct" words or underline them.
export const literalText = {
  autoCapitalize: 'off',
  autoCorrect: 'off',
  spellCheck: false,
} as const
