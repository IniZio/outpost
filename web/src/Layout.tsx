import { Link, Outlet } from "react-router-dom";
import { api } from "./api";
import { isSignedOut, useMe } from "./hooks";

export function Layout() {
  const me = useMe();
  if (isSignedOut(me.error)) return <SignIn />;
  const out = async () => {
    await api.signOut().catch(() => undefined);
    location.assign("/");
  };
  return (
    <>
      <div className="top">
        <Link to="/"><b>Outpost</b></Link>
        <span className="grow" />
        {me.data && (
          <details className="menu">
            <summary className="mut">{me.data.login} ▾</summary>
            <div className="menu-pop card">
              <div className="small mut pad-s">Agents act as {me.data.login} on GitHub.</div>
              <button className="btn" onClick={out}>Sign out</button>
            </div>
          </details>
        )}
      </div>
      <div className="wrap">
        {me.data && !me.data.act && (
          <div className="block">
            This session can only watch. To act from the browser on a local trial, start the server with{" "}
            <code>OUTPOST_DEV_BROWSER_CONTROL=true</code> and sign in again with a new link.
          </div>
        )}
        <Outlet context={{ act: me.data?.act ?? false, login: me.data?.login ?? "" }} />
      </div>
    </>
  );
}

function SignIn() {
  return (
    <div className="wrap">
      <h1>Sign in to Outpost</h1>
      <p className="mut">
        Run <code>outpost-cli weblink</code> and open the link it prints. It signs this browser in.
      </p>
    </div>
  );
}

export type Ctx = { act: boolean; login: string };
