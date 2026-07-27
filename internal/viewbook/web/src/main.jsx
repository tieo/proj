import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import { Sketch } from "./Sketch.jsx";

const SAVE_AFTER_IDLE_MS = 800;

async function api(path, options) {
  const response = await fetch(path, options);
  if (!response.ok) throw new Error(`${options?.method ?? "GET"} ${path}: ${response.status}`);
  return response.json();
}

/** The hash is the address: #/ , #/view/results , #/sketch/results , #/markets . */
function useRoute() {
  const [route, setRoute] = useState(() => location.hash.slice(2) || "");
  useEffect(() => {
    const follow = () => setRoute(location.hash.slice(2) || "");
    window.addEventListener("hashchange", follow);
    return () => window.removeEventListener("hashchange", follow);
  }, []);
  return route;
}

const slug = (uid) => uid.replace(/^VIEW-/, "").toLowerCase();
const statusClass = (status) => `pill ${status.toLowerCase()}`;

function App() {
  const route = useRoute();
  const [model, setModel] = useState(null);
  const [config, setConfig] = useState(null);
  const [saved, setSaved] = useState("");
  const timer = useRef(null);

  const reload = useCallback(() => {
    api("/api/model").then(setModel);
    api("/api/config").then(setConfig);
  }, []);

  useEffect(() => { reload(); }, [reload]);

  // The files are the document and the agent writes to them too, so the page
  // follows them rather than showing whatever it read when it opened.
  //
  // ?static=1 skips it: an open stream never lets a page finish loading, and a
  // screenshot tool waits for exactly that.
  useEffect(() => {
    if (new URLSearchParams(location.search).has("static")) return undefined;
    const stream = new EventSource("/api/events");
    stream.addEventListener("changed", () => {
      reload();
      setSaved("updated");
    });
    return () => stream.close();
  }, [reload]);

  // The model on disk is the document; an edit here is written back after a pause.
  const save = useCallback((next) => {
    setModel(next);
    setSaved("saving…");
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      api("/api/model", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(next),
      })
        .then(() => setSaved("saved"))
        .catch((error) => setSaved(`not saved: ${error.message}`));
    }, SAVE_AFTER_IDLE_MS);
  }, []);

  if (!model || !config) return <div className="loading">Reading the model…</div>;

  const views = model.views;
  const [section, argument] = route.split("/");

  let page;
  if (section === "view") {
    const view = views.find((v) => slug(v.uid) === argument);
    page = view ? <ViewPage model={model} view={view} onChange={save} /> : <NoSuchView />;
  } else if (section === "sketch") {
    page = <Sketch name={argument} title={views.find((v) => slug(v.uid) === argument)?.title} />;
  } else if (section === "table") {
    const table = config.tables.find((t) => t.name === argument);
    page = table ? <TablePage table={table} /> : <NoSuchView what="table" />;
  } else {
    page = <IndexPage views={views} model={model} />;
  }

  return (
    <div className="shell">
      <aside>
        <a className="brand" href="#/">
          {config.title}
          <span>{config.subtitle}</span>
        </a>
        <nav>
          <a className={route === "" ? "on" : ""} href="#/">All views</a>
          {config.tables.map((table) => (
            <a
              key={table.name}
              className={section === "table" && argument === table.name ? "on" : ""}
              href={`#/table/${table.name}`}
            >
              {table.title}
            </a>
          ))}
        </nav>
        <p className="label">Views</p>
        <nav>
          {views.map((view) => (
            <a
              key={view.uid}
              className={section === "view" && argument === slug(view.uid) ? "on" : ""}
              href={`#/view/${slug(view.uid)}`}
            >
              {view.title}
              {view.status === "Missing" && <span className="dot" title="not built" />}
            </a>
          ))}
        </nav>
        <p className="state">{saved}</p>
      </aside>
      <main>{page}</main>
    </div>
  );
}

function NoSuchView({ what = "view" }) {
  return <div className="page"><h1>No such {what}</h1></div>;
}

function requirementsOf(model, uid) {
  const own = model.requirements.filter((r) =>
    r.relations.some((x) => x.to === uid && x.role === "Lives in"));
  return {
    all: own,
    built: own.filter((r) => r.status === "Built").length,
    broken: own.filter((r) => r.status === "Broken").length,
    missing: own.filter((r) => r.status === "Missing").length,
  };
}

function IndexPage({ views, model }) {
  const open = model.requirements.filter((r) => r.status !== "Built");
  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Every view</h1>
          <p>
            {views.length} views · {model.requirements.length} requirements ·{" "}
            {open.length === 0 ? "all built" : `${open.length} not built`}
          </p>
        </div>
      </header>
      <div className="grid">
        {views.map((view) => {
          const own = requirementsOf(model, view.uid);
          return (
            <a className="card" key={view.uid} href={`#/view/${slug(view.uid)}`}>
              <div className="shot">
                {view.screenshot ? (
                  <img src={`/img/card/${view.screenshot}`} alt={view.title} loading="lazy" />
                ) : (
                  <div className="noshot">nothing renders this yet</div>
                )}
              </div>
              <div className="card-body">
                <h2>{view.title}</h2>
                <p>{view.statement.split(".")[0]}.</p>
                <div className="counts">
                  {own.built > 0 && <span className="pill built">{own.built} built</span>}
                  {own.broken > 0 && <span className="pill broken">{own.broken} broken</span>}
                  {own.missing > 0 && <span className="pill missing">{own.missing} missing</span>}
                </div>
              </div>
            </a>
          );
        })}
      </div>
    </div>
  );
}

function ViewPage({ model, view, onChange }) {
  const uid = view.uid;
  const states = model.states.filter((s) =>
    s.relations.some((r) => r.to === uid && r.role === "State of"));
  const requirements = requirementsOf(model, uid).all;
  const stories = useMemo(
    () => Object.fromEntries(model.stories.map((s) => [s.uid, s])), [model.stories]);
  const reachedFrom = view.relations
    .filter((r) => r.role === "Reached from")
    .map((r) => model.views.find((v) => v.uid === r.to))
    .filter(Boolean);

  const editView = (patch) => onChange({
    ...model,
    views: model.views.map((v) => (v.uid === uid ? { ...v, ...patch } : v)),
  });

  const setStatus = (reqUid, status) => onChange({
    ...model,
    requirements: model.requirements.map((r) => (r.uid === reqUid ? { ...r, status } : r)),
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>
            {view.title}
            {view.status === "Missing" && <span className="pill missing">not built</span>}
          </h1>
          <p>{view.statement}</p>
          {reachedFrom.length > 0 && (
            <p className="from">
              Reached from{" "}
              {reachedFrom.map((v, i) => (
                <React.Fragment key={v.uid}>
                  {i > 0 && ", "}
                  <a href={`#/view/${slug(v.uid)}`}>{v.title}</a>
                </React.Fragment>
              ))}
            </p>
          )}
        </div>
        <a className="button" href={`#/sketch/${slug(uid)}`}>Sketch it</a>
      </header>

      <div className="columns">
        <section className="render">
          <h3>As it renders today</h3>
          {view.screenshot ? (
            <img src={`/img/small/${view.screenshot}`} alt={`${view.title} as rendered`} />
          ) : (
            <div className="noshot tall">
              <span>Nothing renders this yet.</span>
            </div>
          )}
        </section>

        <div className="detail">
          <section>
            <h3>What it has to do</h3>
            <ul className="reqs">
              {requirements.map((req) => {
                const served = req.relations
                  .filter((r) => r.role === "Fulfils")
                  .map((r) => stories[r.to]?.title)
                  .filter(Boolean);
                return (
                  <li key={req.uid}>
                    <div className="req-head">
                      <span className={statusClass(req.status)}>{req.status}</span>
                      <strong>{req.title}</strong>
                    </div>
                    <p>{req.statement}</p>
                    {served.length > 0 && <p className="serves">Serves: {served.join(" · ")}</p>}
                    <div className="setstatus">
                      {["Built", "Broken", "Missing"].map((s) => (
                        <button
                          key={s}
                          className={req.status === s ? "on" : ""}
                          onClick={() => setStatus(req.uid, s)}
                        >
                          {s}
                        </button>
                      ))}
                    </div>
                  </li>
                );
              })}
              {requirements.length === 0 && <li className="empty">Nothing recorded yet.</li>}
            </ul>
          </section>

          {states.length > 0 && (
            <section>
              <h3>States it can be in</h3>
              <ul className="states">
                {states.map((state) => (
                  <li key={state.uid}>
                    <strong>{state.title}</strong>
                    <p>{state.statement}</p>
                  </li>
                ))}
              </ul>
            </section>
          )}

          <section>
            <h3>Notes</h3>
            <textarea
              value={view.notes ?? ""}
              placeholder="What you want changed here. Written straight into model.json."
              onChange={(e) => editView({ notes: e.target.value })}
            />
          </section>
        </div>
      </div>
    </div>
  );
}

/**
 * A table a project declares in viewbook.json: where the rows come from, which columns to show,
 * and how to say a value that is a list or a flag. Nothing here knows what the rows are about.
 */
function TablePage({ table }) {
  const [data, setData] = useState(null);
  useEffect(() => {
    setData(null);
    api(`/api/table/${table.name}`).then(setData).catch(() => setData({}));
  }, [table.name]);

  if (!data) return <div className="page"><h1>{table.title}</h1><p>Reading…</p></div>;

  const rows = table.rows ? (data[table.rows] ?? []) : (Array.isArray(data) ? data : []);
  const sorted = table.sortBy
    ? [...rows].sort((a, b) => String(a[table.sortBy]).localeCompare(String(b[table.sortBy])))
    : rows;

  const cell = (row, column) => {
    const value = row[column.field];
    if (Array.isArray(value)) return value.length ? value.join(", ").toLowerCase() : "—";
    if (typeof value === "boolean") return value ? (column.yes ?? "yes") : (column.no ?? "—");
    if (value === null || value === undefined || value === "") return column.empty ?? "—";
    return String(value);
  };

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>{table.title}</h1>
          {table.statement && <p>{table.statement}</p>}
        </div>
      </header>
      <table className="table">
        <thead>
          <tr>{table.columns.map((c) => <th key={c.field}>{c.title}</th>)}</tr>
        </thead>
        <tbody>
          {sorted.map((row, index) => (
            <tr key={row.id ?? index}>
              {table.columns.map((column, position) => (
                <td key={column.field}>
                  {position === 0 ? <strong>{cell(row, column)}</strong> : cell(row, column)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

createRoot(document.getElementById("root")).render(<App />);
