import type * as pdfjs from "pdfjs-dist";

/** Why pdf.js is asking: the document is locked, or the password just typed was wrong. */
export type PasswordRequest = "required" | "incorrect";

/** Resolves to the typed password, or null when the reader gave up. */
export type AskPassword = (request: PasswordRequest) => Promise<string | null>;

/**
 * Let pdf.js ask for a password instead of failing the document.
 *
 * A task that needs a password and has no `onPassword` handler rejects with `PasswordException`,
 * and the viewer — which creates the task itself, deep inside its renderer — turns that into a
 * "download it and open it elsewhere" card. Handing the task a handler here is therefore the one
 * place where a password can enter: pdf.js keeps the promise pending until the callback supplies
 * one, so the viewer never sees the rejection and renders the document it would have rendered for
 * an unencrypted file, toolbar and all. Nothing about the file changes; pdf.js decrypts in memory.
 */
export function attachPasswordPrompt<T extends pdfjs.PDFDocumentLoadingTask>(task: T, ask: AskPassword): T {
  task.onPassword = (updatePassword: (password: string) => void, reason: number) => {
    void ask(reason === 2 ? "incorrect" : "required").then((password) => {
      if (password === null) {
        // Cancelling has to end the task: left pending, the viewer would spin forever.
        void task.destroy();
        return;
      }
      updatePassword(password);
    });
  };
  return task;
}

/**
 * The pdf.js module the viewer should use: identical except that every document it opens can ask
 * for its password. The viewer exposes this as the `pdfjs` plugin option, which is what makes
 * reusing it possible instead of rendering PDFs a second time.
 */
export function pdfWithPasswordPrompt(module: typeof pdfjs, ask: AskPassword): typeof pdfjs {
  return {
    ...module,
    getDocument(...args: Parameters<typeof module.getDocument>) {
      return attachPasswordPrompt(module.getDocument(...args), ask);
    },
  };
}
