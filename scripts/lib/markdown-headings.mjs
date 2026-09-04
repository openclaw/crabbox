export function reserveHeadingAnchor(anchors, base) {
  let anchor = base;
  let suffix = 0;
  while (anchors.has(anchor)) {
    suffix += 1;
    anchor = `${base}-${suffix}`;
  }
  anchors.add(anchor);
  return anchor;
}
