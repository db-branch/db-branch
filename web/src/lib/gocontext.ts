import { createContext } from "react";

// GoContext moves the console to another address without a page load. The shell
// provides it; outside the shell (a test, a lone page) it loads the address.
export const GoContext = createContext<(href: string) => void>((href) => {
  location.assign(href);
});
