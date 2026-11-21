// ui/embed.go embeds dist/, which must exist for `go build` to compile even
// before anyone builds the UI. git keeps dist/.gitkeep for that; vite empties
// dist on every build, so put it back.
import { writeFileSync } from "node:fs";
writeFileSync(new URL("../dist/.gitkeep", import.meta.url), "");
