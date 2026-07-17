(boolean_scalar) @boolean
(null_scalar) @constant.builtin
[
  (double_quote_scalar)
  (single_quote_scalar)
  (block_scalar)
  (string_scalar)
] @string
[
  (integer_scalar)
  (float_scalar)
] @number
(comment) @comment
[
  (anchor_name)
  (alias_name)
] @label
(tag) @type
(block_mapping_pair
  key: (flow_node
    [
      (double_quote_scalar)
      (single_quote_scalar)
    ] @property))
(block_mapping_pair
  key: (flow_node
    (plain_scalar
      (string_scalar) @property)))
[
  ","
  "-"
  ":"
  ">"
  "?"
  "|"
] @punctuation.delimiter
[
  "["
  "]"
  "{"
  "}"
] @punctuation.bracket
[
  "*"
  "&"
  "---"
  "..."
] @punctuation.special
