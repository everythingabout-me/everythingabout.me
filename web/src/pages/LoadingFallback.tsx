import { BrandMark } from "../components/BrandMark";
import styles from "../styles/auth.module.css";

export function LoadingFallback() {
  return (
    <div className={styles.page}>
      <main className={styles.main}>
        <div className={styles.authStack} aria-busy="true" aria-live="polite">
          <BrandMark size={32} className={styles.loadingMark} />
          <span className={styles.pageSwitch}>加载中…</span>
        </div>
      </main>
    </div>
  );
}
