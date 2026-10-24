import { Link, Outlet } from "react-router";
import styles from "../styles/auth.module.css";

export function AuthLayout() {
  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <Link to="/" className={styles.brand}>
          <span className={styles.brandDot} />
          <span>Everything About Me</span>
        </Link>
      </header>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  );
}
