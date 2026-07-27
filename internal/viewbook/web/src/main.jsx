import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import { Sketch } from "./Sketch.jsx";

const SAVE_AFTER_IDLE_MS = 800;

/** Where this book is mounted: "/" when it is alone, "/arbay/" when it is one of several. */
export const base = location.pathname.replace(/[^/]*$/, "");

async function api(path, options) {
  const response = await fetch(base + path, options);
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
    api("api/model").then(setModel);
    api("api/config").then(setConfig);
  }, []);

  useEffect(() => { reload(); }, [reload]);

  // The files are the document and the agent writes to them too, so the page
  // follows them rather than showing whatever it read when it opened.
  //
  // ?static=1 skips it: an open stream never lets a page finish loading, and a
  // screenshot tool waits for exactly that.
  useEffect(() => {
    if (new URLSearchParams(location.search).has("static")) return undefined;
    const stream = new EventSource(base + "api/events");
    stream.addEventListener("changed", () => {
      reload();
      setSaved("updated");
    });
    return () => stream.close();
  }, [reload]);

  // The model on disk is the document; an edit here is written back after a pause.
  const save = useCallback((next, announce = false) => {
    setModel(next);
    setSaved("saving…");
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      api("api/model", {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          ...(announce ? { "X-Viewbook-Announce": "1" } : {}),
        },
        // Written to the file as sent, so a changed note is a changed line.
        body: JSON.stringify(next, null, 2),
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

  // One bar across the top rather than a column down the side: a screen in
  // landscape has width to spare and height to save, and a phone has neither to
  // give to a permanent list.
  //
  // The book's name is the way back to the index, so nothing else has to be.
  return (
    <div className="shell">
      <header className="bar">
        <a className={`brand ${route === "" ? "on" : ""}`} href="#/">
          {config.title}
          {config.subtitle && <span>{config.subtitle}</span>}
        </a>
        <nav className="tabs">
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
          {config.tables.map((table) => (
            <a
              key={table.name}
              className={`table-tab ${section === "table" && argument === table.name ? "on" : ""}`}
              href={`#/table/${table.name}`}
            >
              {table.title}
            </a>
          ))}
        </nav>
        <span className="state">{saved}</span>
      </header>
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

/**
 * What a view looks like today, at whatever shape it actually is.
 *
 * The same screen is upright on a phone and wide on a desktop, and a view can
 * carry both. Which one an image is, is measured when it loads rather than
 * declared in the model, so a project only has to drop the file in img/.
 */
function Render({ view, onShape }) {
  const shots = rendersOf(view);
  const [chosen, setChosen] = useState(0);

  useEffect(() => setChosen(0), [view.uid]);

  if (shots.length === 0) {
    return <div className="noshot tall"><span>Nothing renders this yet.</span></div>;
  }
  const shot = shots[Math.min(chosen, shots.length - 1)];
  return (
    <>
      <div className="frame">
        <img
          src={`${base}img/small/${shot.file}`}
          alt={`${view.title} as rendered`}
          onLoad={(e) => onShape(e.target.naturalWidth > e.target.naturalHeight ? "landscape" : "portrait")}
        />
      </div>
      {shots.length > 1 && (
        <div className="shapes">
          {shots.map((one, index) => (
            <button
              key={one.file}
              className={index === chosen ? "on" : ""}
              onClick={() => setChosen(index)}
            >
              {one.label ?? one.file.replace(/\.[a-z]+$/, "")}
            </button>
          ))}
        </div>
      )}
    </>
  );
}

/** Every render a view carries: one screenshot, or a list of them. */
function rendersOf(view) {
  if (Array.isArray(view.renders) && view.renders.length > 0) {
    return view.renders.map((one) => (typeof one === "string" ? { file: one } : one));
  }
  return view.screenshot ? [{ file: view.screenshot }] : [];
}

/** A thumbnail, cropped when it is a tall screen and shown whole when it is wide. */
function Thumb({ view }) {
  const [shape, setShape] = useState("portrait");
  const shots = rendersOf(view);
  if (shots.length === 0) return <div className="noshot">nothing renders this yet</div>;
  return (
    <img
      className={shape}
      src={`${base}img/card/${shots[0].file}`}
      alt={view.title}
      loading="lazy"
      onLoad={(e) => setShape(e.target.naturalWidth > e.target.naturalHeight ? "landscape" : "portrait")}
    />
  );
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
                <Thumb view={view} />
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
      <SketchBox />
    </div>
  );
}

/**
 * Sketching, which is for a screen that does not exist yet.
 *
 * It sits with the index rather than on a view: a view that already renders has
 * a render, and drawing over it says nothing the screenshot does not.
 */
function SketchBox() {
  const [drawn, setDrawn] = useState([]);
  const [name, setName] = useState("");
  useEffect(() => {
    api("api/sketches").then(setDrawn).catch(() => setDrawn([]));
  }, []);

  const start = () => {
    const called = name.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
    if (called) location.hash = `#/sketch/${called}`;
  };

  return (
    <section className="sketches">
      <h3>Sketches</h3>
      <div className="sketch-row">
        {drawn.map((one) => (
          <a className="button" key={one} href={`#/sketch/${one}`}>{one}</a>
        ))}
        <input
          value={name}
          placeholder="a screen that does not exist yet"
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && start()}
        />
        <button className="send" onClick={start} disabled={!name.trim()}>Sketch it</button>
      </div>
    </section>
  );
}

/** Whether this screen has room for anything beyond the render and the conversation. */
const roomToSpare = () => window.matchMedia("(min-width: 1000px) and (min-height: 800px)").matches;

function ViewPage({ model, view, onChange }) {
  const uid = view.uid;
  const [shape, setShape] = useState("portrait");
  const [more, setMore] = useState(roomToSpare);
  const states = model.states.filter((s) =>
    s.relations.some((r) => r.to === uid && r.role === "State of"));
  const requirements = requirementsOf(model, uid).all;
  const stories = useMemo(
    () => Object.fromEntries(model.stories.map((s) => [s.uid, s])), [model.stories]);
  const reachedFrom = view.relations
    .filter((r) => r.role === "Reached from")
    .map((r) => model.views.find((v) => v.uid === r.to))
    .filter(Boolean);

  const editView = (patch, announce) => onChange({
    ...model,
    views: model.views.map((v) => (v.uid === uid ? { ...v, ...patch } : v)),
  }, announce);

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
      </header>

      {/* Three things fill the screen: how the view renders, what is being
          asked about it, and what came back. An upright render stands beside
          the conversation; a wide one takes the width and the conversation goes
          under it. Everything else is behind the fold, and on a screen with no
          room for it, out of the way entirely. */}
      <div className={`work ${shape}`}>
        <section className="render">
          <Render view={view} onShape={setShape} />
        </section>
        <Ask about={view.title} />
      </div>

      <details className="more" open={more} onToggle={(e) => setMore(e.target.open)}>
        <summary>What it has to do{states.length > 0 && ", the states it can be in"}, and notes</summary>
        <p className="statement">{view.statement}</p>
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
              placeholder="Kept with the view. Saved as you type; sent to the session when you click away."
              onChange={(e) => editView({ notes: e.target.value }, false)}
              onBlur={() => editView({ notes: view.notes ?? "" }, true)}
            />
          </section>
        </div>
      </details>
    </div>
  );
}

/**
 * A table a project declares in viewbook.json: where the rows come from, which columns to show,
 * and how to say a value that is a list or a flag. Nothing here knows what the rows are about.
 */
/**
 * Say something to the session, and see it land.
 *
 * The page had a notes box that saved silently, so asking for something looked
 * exactly like doing nothing: no button, no confirmation, no reply. This is the
 * other half of the loop - what is typed goes to the conversation, and what the
 * conversation says comes back underneath.
 */
function Ask({ about }) {
  const [text, setText] = useState("");
  const [shots, setShots] = useState([]);
  const [state, setState] = useState("");
  const [reply, setReply] = useState("");
  const talk = useRef(null);
  // The conversation is there whether or not this page asked the last question,
  // so it is shown on arrival rather than only after sending something.


  const send = () => {
    const message = text.trim();
    if (!message && shots.length === 0) return;
    setState("sending…");
    // A pasted image is already a file in the project; the conversation is given
    // its path, which is something it can actually open.
    const attached = shots.map((s) => `\n${s.path}`).join("");
    const said = (about ? `About ${about}: ${message}` : message) + attached;
    api("api/say", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: said }),
    })
      .then(() => {
        setState("sent to the session");
        setText("");
        setShots([]);
      })
      .catch((error) => setState(`not sent: ${error.message}`));
  };

  const paste = (event) => {
    const images = [...(event.clipboardData?.items ?? [])]
      .filter((item) => item.type.startsWith("image/"));
    if (images.length === 0) return;
    event.preventDefault();
    setState("keeping the image…");
    images.forEach((item) => {
      const file = item.getAsFile();
      if (!file) return;
      fetch(base + "api/paste", {
        method: "POST",
        headers: { "Content-Type": file.type },
        body: file,
      })
        .then((r) => r.json())
        .then((kept) => {
          setShots((now) => [...now, kept]);
          setState("image kept; it goes with the message");
        })
        .catch((error) => setState(`image not kept: ${error.message}`));
    });
  };

  // The newest line is the one being waited for, so the pane stays at its end.
  useEffect(() => {
    if (talk.current) talk.current.scrollTop = talk.current.scrollHeight;
  }, [reply]);

  // After saying something, follow the session for a while so the answer shows
  // up here rather than only in a terminal somewhere.
  useEffect(() => {
    const read = () => api("api/session").then((r) => setReply(r.text || "")).catch(() => {});
    read();
    const every = setInterval(read, 2500);
    return () => clearInterval(every);
  }, []);

  // The conversation stands beside the render, because those two and what is
  // typed between them are what the page is for.
  return (
    <section className="talk">
      {reply
        ? <pre className="reply" ref={talk}>{reply.trimEnd().split("\n").slice(-80).join("\n")}</pre>
        : <p className="hint">Nothing in the session yet.</p>}
      <div className="dock-row">
      <textarea
        className="ask"
        value={text}
        placeholder={`Tell the session what to change about ${about ?? "this"}. Paste a screenshot if it helps. Enter sends, Shift+Enter is a newline.`}
        onChange={(e) => setText(e.target.value)}
        onPaste={paste}
        // Enter sends, because this is a message rather than a document.
        // Shift+Enter is the newline.
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault();
            send();
          }
        }}
      />
      {shots.length > 0 && (
        <div className="shots">
          {shots.map((shot) => (
            <span key={shot.url}>
              <img src={base + shot.url} alt="pasted" />
              <button className="quiet" onClick={() => setShots((now) => now.filter((s) => s !== shot))}>
                remove
              </button>
            </span>
          ))}
        </div>
      )}
        <button
          className="send"
          onClick={send}
          disabled={!text.trim() && shots.length === 0}
        >
          Send
        </button>
      </div>
      <div className="ask-bar">
        <span className={state.startsWith("not sent") ? "bad" : ""}>{state}</span>
      </div>
    </section>
  );
}

function TablePage({ table }) {
  const [data, setData] = useState(null);
  useEffect(() => {
    setData(null);
    api(`api/table/${table.name}`).then(setData).catch(() => setData({}));
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
