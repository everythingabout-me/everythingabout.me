import { Link } from "react-router";
import { BrandMark } from "../components/BrandMark";
import { t, type MessageKey } from "../i18n";
import { legalDocument, type LegalDocumentKey } from "./legal/documents";
import styles from "./LegalPage.module.css";

const titles: Record<LegalDocumentKey, MessageKey> = {
  privacy: "legal.privacy",
  terms: "legal.terms",
};

/** A public document page: no account required, so it can be linked from anywhere. */
export function LegalPage({ documentKey }: { documentKey: LegalDocumentKey }) {
  const doc = legalDocument(documentKey);
  const title = t(titles[documentKey]);
  const otherKey: LegalDocumentKey = documentKey === "privacy" ? "terms" : "privacy";

  return (
    <div className={styles.page}>
      <title>{`${title} · ${t("app.name")}`}</title>
      <header className={styles.header}>
        <Link to="/" className={styles.brand}>
          <BrandMark className={styles.brandMark} />
          <span>{t("app.name")}</span>
        </Link>
        <Link to="/" className={styles.back}>
          {t("legal.backHome")}
        </Link>
      </header>
      <main className={styles.main}>
        <article className={styles.article}>
          <h1 className={styles.title}>{title}</h1>
          <p className={styles.updated}>{t("legal.updated", { date: doc.updated })}</p>
          {doc.intro.map((paragraph, index) => (
            <p key={index} className={styles.lead}>
              {paragraph}
            </p>
          ))}
          {doc.sections.map((section) => (
            <section key={section.heading} className={styles.section}>
              <h2 className={styles.heading}>{section.heading}</h2>
              {section.paragraphs.map((paragraph, index) => (
                <p key={index}>{paragraph}</p>
              ))}
              {section.bullets ? (
                <ul className={styles.list}>
                  {section.bullets.map((bullet, index) => (
                    <li key={index}>{bullet}</li>
                  ))}
                </ul>
              ) : null}
            </section>
          ))}
          <footer className={styles.footer}>
            <Link to={`/${otherKey}`} className={styles.footerLink}>
              {t(titles[otherKey])}
            </Link>
            <Link to="/" className={styles.footerLink}>
              {t("legal.backHome")}
            </Link>
          </footer>
        </article>
      </main>
    </div>
  );
}
