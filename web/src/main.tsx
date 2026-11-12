import "@fontsource/ibm-plex-sans/400.css";
import "@fontsource/ibm-plex-sans/500.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "./styles.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ADMIN, App } from "./App";
import { chooseLanguage, remembered, setLanguage } from "./lib/i18n";
import { Portal } from "./pages/Portal";

// Until the session says otherwise: this browser's choice, else its language.
setLanguage(chooseLanguage(remembered(), navigator.language));

// Admin pages used to live at the root: keep old links working.
const legacy = /^\/(branches|bases|cluster|settings|m)(\/|$)/;
if (legacy.test(location.pathname)) history.replaceState(null, "", ADMIN + location.pathname + location.search);

const admin = location.pathname === ADMIN || location.pathname.startsWith(ADMIN + "/");

const render = () => createRoot(document.getElementById("root")!).render(<StrictMode>{admin ? <App /> : <Portal />}</StrictMode>);

// The static demo (vite build --mode demo) puts a fake agent in the browser before the console starts.
// In every other build this condition is false at build time and the demo code is not included.
if (import.meta.env.MODE === "demo") void import("./demo/install").then((m) => (m.installDemo(), render()));
else render();
