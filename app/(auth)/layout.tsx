import Link from "next/link";
import styles from "./auth.module.css";

export default function AuthLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <Link href="/" className={styles.brand}>
          <span className={styles.brandDot} />
          <span>Everything About Me</span>
        </Link>
      </header>
      <main className={styles.main}>{children}</main>
    </div>
  );
}
