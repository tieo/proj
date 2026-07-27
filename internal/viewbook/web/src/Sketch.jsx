import React, { useCallback, useEffect, useRef, useState } from "react";
import { Excalidraw, MainMenu } from "@excalidraw/excalidraw";
import "@excalidraw/excalidraw/index.css";

const SAVE_AFTER_IDLE_MS = 1200;

/**
 * A canvas for one view, and only for drawing.
 *
 * The model itself is a document and reads like one; a wireframe is the one thing that genuinely
 * needs a canvas, so it lives behind its own address rather than being the container everything
 * else has to be panned around inside.
 */
export function Sketch({ name, title }) {
  const [scene, setScene] = useState(null);
  const [saved, setSaved] = useState("loaded");
  const timer = useRef(null);
  const last = useRef(null);

  useEffect(() => {
    let alive = true;
    fetch(`/api/sketch/${name}`)
      .then((r) => r.json())
      .then((data) => {
        if (!alive) return;
        last.current = JSON.stringify(data.elements ?? []);
        setScene(data);
      });
    return () => {
      alive = false;
    };
  }, [name]);

  const onChange = useCallback(
    (elements, appState, files) => {
      const serialized = JSON.stringify(elements);
      if (serialized === last.current) return;
      last.current = serialized;
      setSaved("saving…");
      clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        fetch(`/api/sketch/${name}`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            type: "excalidraw",
            version: 2,
            source: "arbay-model",
            elements,
            appState: {
              viewBackgroundColor: appState.viewBackgroundColor,
              gridSize: appState.gridSize,
            },
            files,
          }),
        })
          .then(() => setSaved("saved"))
          .catch((error) => setSaved(`not saved: ${error.message}`));
      }, SAVE_AFTER_IDLE_MS);
    },
    [name],
  );

  return (
    <div className="sketch">
      <header className="page-head">
        <div>
          <h1>{title ?? name} — sketch</h1>
          <p>Drawn here, saved to docs/model/wireframes/{name}.excalidraw. {saved}</p>
        </div>
        <a className="button" href={`#/view/${name}`}>Back to the view</a>
      </header>
      <div className="canvas">
        {scene && (
          <Excalidraw
            initialData={{ ...scene, scrollToContent: true }}
            onChange={onChange}
            UIOptions={{ canvasActions: { loadScene: false, saveToActiveFile: false } }}
          >
            <MainMenu>
              <MainMenu.DefaultItems.ToggleTheme />
              <MainMenu.DefaultItems.ChangeCanvasBackground />
              <MainMenu.DefaultItems.SaveAsImage />
            </MainMenu>
          </Excalidraw>
        )}
      </div>
    </div>
  );
}
