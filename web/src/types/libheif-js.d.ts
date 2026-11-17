/**
 * `libheif-js` ships no types for its ESM wasm bundle.
 *
 * The HEIC plugin imports it dynamically and narrows the result to the three calls it makes, so
 * this declaration only has to describe the Emscripten factory it actually calls.
 */
declare module "libheif-js/libheif-wasm/libheif-bundle.mjs" {
  const createModule: () => Promise<unknown>;
  export default createModule;
}
