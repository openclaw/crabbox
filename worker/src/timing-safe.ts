export function timingSafeEqual(a: string, b: string): boolean {
  let difference = a.length ^ b.length;
  const length = Math.max(a.length, b.length);

  for (let index = 0; index < length; index += 1) {
    difference |= (a.charCodeAt(index) | 0) ^ (b.charCodeAt(index) | 0);
  }

  return difference === 0;
}
