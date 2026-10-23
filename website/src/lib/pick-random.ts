/**
 * Fisher–Yates shuffle, returning a new array with at most `count` items.
 * The `random` parameter is injectable so tests can be deterministic.
 */
export function pickRandom<T>(
  items: readonly T[],
  count: number,
  random: () => number = Math.random,
): T[] {
  if (count <= 0) {
    return [];
  }
  const copy = [...items];
  if (copy.length <= count) {
    return copy;
  }
  for (let index = copy.length - 1; index > 0; index -= 1) {
    const swapWith = Math.floor(random() * (index + 1));
    const current = copy[index];
    copy[index] = copy[swapWith];
    copy[swapWith] = current;
  }
  return copy.slice(0, count);
}
