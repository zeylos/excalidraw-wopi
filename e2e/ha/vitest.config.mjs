import path from "node:path";
import { defineConfig } from "vitest/config";

// Vitest's default file order is not filename-alphabetical (its glob
// collection order does not match the 01/02/03/04 prefixes here), and
// `test.include` as a literal array does not preserve that array's
// order either. 04-failover permanently kills one instance, so it must
// run strictly after every test that still needs both instances up;
// this custom sequencer is the supported way to force that order.
const fileOrder = ["01-presence.test.mjs", "02-write.test.mjs", "03-distribution.test.mjs", "04-failover.test.mjs"];

// rank puts a file absent from fileOrder last, instead of first: a plain
// indexOf(-1) would otherwise sort an unknown file ahead of every known
// one, which risks running it before the instances it needs are ready.
function rank(moduleId) {
  const i = fileOrder.indexOf(path.basename(moduleId));
  return i === -1 ? Number.MAX_SAFE_INTEGER : i;
}

class FixedOrderSequencer {
  async shard(files) {
    return files;
  }
  async sort(files) {
    return [...files].sort((a, b) => rank(a.moduleId) - rank(b.moduleId));
  }
}

export default defineConfig({
  test: {
    globalSetup: "./global-setup.mjs",
    // globalSetup builds bin/excalidraw-wopi (a full frontend build on a
    // clean checkout) and starts two instances plus the wopihost and the
    // hashproxy, well past vitest's 20s default hook timeout.
    testTimeout: 20000,
    hookTimeout: 180000,
    fileParallelism: false,
    sequence: { sequencer: FixedOrderSequencer },
  },
});
