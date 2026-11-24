import journal from "./meta/_journal.json";
import m0000 from "./0000_fearless_leper_queen.sql";
import m0001 from "./0001_good_talos.sql";
import m0002 from "./0002_sad_jasper_sitwell.sql";
import m0003 from "./0003_sleepy_psylocke.sql";

export default {
	journal,
	migrations: {
		m0000,
		m0001,
		m0002,
		m0003,
	},
};
