// Seams the static demo build fills in (see demo/install.ts). In the real
// console they stay empty, so nothing here changes how it behaves.
export type FrameProps = { src: string } | { srcDoc: string };

export const hooks: {
  /** Replaces the hint under the password field on the login form. */
  loginHint?: () => string;
  /** Replaces what a module page frame loads. */
  frame?: (module: string, path: string, lang: string) => FrameProps;
} = {};

/** moduleFrame is what an <iframe> showing a module's page loads: the module's own page, served by the agent. */
export function moduleFrame(module: string, path: string, lang: string): FrameProps {
  return hooks.frame?.(module, path, lang) ?? { src: `/v1/ext/${module}${path}?lang=${lang}` };
}
