import { ButtonItem, PanelSection, PanelSectionRow, TextField, staticClasses } from "@decky/ui";
import { callable, definePlugin } from "@decky/api";
import { useEffect, useState } from "react";
import { FaSyncAlt } from "react-icons/fa";

type Result = { ok: boolean; message: string };

const refresh = callable<[], Result>("refresh");
const getProxyUrl = callable<[], string>("get_proxy_url");
const setProxyUrl = callable<[url: string], Result & { url: string }>("set_proxy_url");

function Content() {
  const [result, setResult] = useState<Result>();
  const [busy, setBusy] = useState(false);
  const [url, setUrl] = useState("");
  const [urlError, setUrlError] = useState<string>();

  useEffect(() => {
    getProxyUrl().then(setUrl);
  }, []);

  const save = async () => {
    const r = await setProxyUrl(url);
    setUrl(r.url);
    setUrlError(r.ok ? undefined : r.message);
  };

  return (
    <>
      <PanelSection title="Store proxy">
        <PanelSectionRow>
          <ButtonItem
            layout="below"
            disabled={busy}
            description={result?.message}
            onClick={async () => {
              setBusy(true);
              try {
                setResult(await refresh());
              } finally {
                setBusy(false);
              }
            }}
          >
            {busy ? "Refreshing…" : "Refresh plugins"}
          </ButtonItem>
        </PanelSectionRow>
      </PanelSection>
      <PanelSection title="Settings">
        <PanelSectionRow>
          <TextField
            label="Proxy URL"
            description={urlError ?? "Default: http://127.0.0.1:8737"}
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            onBlur={save}
          />
        </PanelSectionRow>
      </PanelSection>
    </>
  );
}

export default definePlugin(() => ({
  name: "Store Proxy",
  titleView: <div className={staticClasses.Title}>Store Proxy</div>,
  content: <Content />,
  icon: <FaSyncAlt />,
}));
