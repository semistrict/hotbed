/** A small seeded PRNG (mulberry32), so the mock cluster starts the same every load. */
export class Rng {
  private s: number;

  constructor(seed: number) {
    this.s = seed >>> 0;
  }

  next(): number {
    this.s = (this.s + 0x6d2b79f5) >>> 0;
    let t = this.s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  }

  range(lo: number, hi: number): number {
    return lo + (hi - lo) * this.next();
  }

  int(lo: number, hi: number): number {
    return Math.floor(this.range(lo, hi + 1));
  }

  pick<T>(xs: readonly T[]): T {
    const x = xs[Math.floor(this.next() * xs.length)];
    if (x === undefined) throw new Error("pick from empty list");
    return x;
  }

  chance(p: number): boolean {
    return this.next() < p;
  }

  hex(n: number): string {
    let s = "";
    for (let i = 0; i < n; i++) s += Math.floor(this.next() * 16).toString(16);
    return s;
  }

  /** A log-normal draw around median m with spread sigma. */
  logNormal(m: number, sigma: number): number {
    const u = Math.max(1e-9, this.next());
    const v = this.next();
    const z = Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
    return m * Math.exp(sigma * z);
  }
}
