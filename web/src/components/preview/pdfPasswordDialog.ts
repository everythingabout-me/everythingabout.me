import { t } from "../../i18n";
import type { PasswordRequest } from "./pdfPassword";
import styles from "./pdfPasswordDialog.module.css";

// The dialog is a native <dialog>, so focus is trapped, Escape closes it, and it paints above the
// viewer without a z-index race — the viewer is a third-party widget that renders into its own
// container, and an overlay positioned by us would have to out-guess it.
let showing: HTMLDialogElement | null = null;

/**
 * Ask for the password of a PDF that could not be opened without one.
 *
 * Resolves to what the reader typed, or null when they close the dialog; the caller ends the
 * loading task in that case. The password is handed straight to pdf.js and is never kept: there is
 * no "remember" state here on purpose, because a remembered password is a secret this app would
 * have to store, and the app's whole storage model treats anything it stores as reproducible.
 */
export function askPdfPassword(request: PasswordRequest): Promise<string | null> {
  // Only one preview is open at a time; closing the previous dialog ends its own task through the
  // "close" listener below instead of leaving an orphaned prompt behind the new one.
  showing?.close();

  const dialog = document.createElement("dialog");
  dialog.className = styles.dialog;
  dialog.setAttribute("aria-labelledby", "pdf-password-title");

  const form = document.createElement("form");
  form.className = styles.form;
  form.noValidate = true;

  const title = document.createElement("h2");
  title.id = "pdf-password-title";
  title.className = styles.title;
  title.textContent = t("pdf.passwordTitle");

  const text = document.createElement("p");
  text.className = styles.text;
  text.textContent = t("pdf.passwordText");

  const input = document.createElement("input");
  input.type = "password";
  input.className = styles.input;
  input.autocomplete = "off";
  input.spellcheck = false;
  input.placeholder = t("pdf.passwordPlaceholder");
  input.setAttribute("aria-label", t("pdf.passwordLabel"));
  input.setAttribute("aria-invalid", String(request === "incorrect"));

  const error = document.createElement("p");
  error.className = styles.error;
  error.setAttribute("role", "alert");
  error.textContent = t("pdf.wrongPassword");
  error.hidden = request !== "incorrect";

  const actions = document.createElement("div");
  actions.className = styles.actions;

  const cancel = document.createElement("button");
  cancel.type = "button";
  cancel.className = styles.cancel;
  cancel.textContent = t("common.cancel");

  const confirm = document.createElement("button");
  confirm.type = "submit";
  confirm.className = styles.confirm;
  confirm.textContent = t("pdf.open");
  confirm.disabled = true;

  actions.append(cancel, confirm);
  form.append(title, text, input, error, actions);
  dialog.append(form);
  document.body.append(dialog);
  showing = dialog;

  return new Promise<string | null>((resolve) => {
    let settled = false;
    const finish = (password: string | null) => {
      if (settled) return;
      settled = true;
      if (showing === dialog) showing = null;
      dialog.close();
      dialog.remove();
      resolve(password);
    };

    input.addEventListener("input", () => {
      confirm.disabled = input.value.length === 0;
    });
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      if (input.value.length > 0) finish(input.value);
    });
    cancel.addEventListener("click", () => finish(null));
    // Escape fires "cancel" first; without this the browser closes the dialog and the promise
    // would wait on a "close" that this handler still receives — handled once, by `finish`.
    dialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      finish(null);
    });
    dialog.addEventListener("close", () => finish(null));

    dialog.showModal();
    input.focus();
  });
}
