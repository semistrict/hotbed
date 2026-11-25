import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { useSnapshot } from "./data";
import { scope } from "./projects";

type Choice = [project: string | null, choose: (project: string | null) => void];

const ProjectContext = createContext<Choice>([null, () => {}]);

const KEY = "hotbed-project";

/**
 * The project the cockpit shows, null for every project. It is this
 * viewer's own choice, kept in the browser between visits.
 */
export function ProjectProvider({ children }: { children: ReactNode }) {
  const [project, setProject] = useState<string | null>(() => {
    try {
      return localStorage.getItem(KEY);
    } catch {
      return null;
    }
  });
  const choose = useCallback((p: string | null) => {
    setProject(p);
    try {
      if (p === null) localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, p);
    } catch {
      // Private windows refuse storage; the choice holds for this visit.
    }
  }, []);
  const value = useMemo<Choice>(() => [project, choose], [project, choose]);
  return <ProjectContext.Provider value={value}>{children}</ProjectContext.Provider>;
}

/** The project shown (null: every project, or one that is gone), and how to show another. */
export function useProject(): Choice {
  const [project, choose] = useContext(ProjectContext);
  const { data } = useSnapshot();
  const known = project !== null && (data === undefined || (data.projects?.some((p) => p.name === project) ?? false));
  return [known ? project : null, choose];
}

/** The snapshot as the project shown sees it. */
export function useScopedSnapshot() {
  const q = useSnapshot();
  const [project] = useProject();
  const data = useMemo(() => (q.data ? scope(q.data, project) : undefined), [q.data, project]);
  return { ...q, data };
}
