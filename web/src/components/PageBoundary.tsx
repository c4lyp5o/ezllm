import { Component, type ReactNode } from 'react';
import { Spinner, btn, cx } from './ui';

// Pages load on demand (App.tsx). PageLoading is the brief state while a
// page's code arrives; PageBoundary catches a page whose code failed to load
// — most often a dashboard open across a deploy, whose old chunk names no
// longer exist — and offers a reload instead of a blank screen.

export function PageLoading() {
  return (
    <div className="flex items-center gap-2 py-16 text-[13px] text-mute" role="status">
      <Spinner /> Loading…
    </div>
  );
}

type Props = { children: ReactNode };
type State = { failed: boolean };

export class PageBoundary extends Component<Props, State> {
  state: State = { failed: false };

  static getDerivedStateFromError(): State {
    return { failed: true };
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <div className="flex flex-col items-start gap-3 py-16 text-[13px]" role="alert">
        <p className="text-ink">This page could not load. The dashboard may have been updated since you opened it.</p>
        <button type="button" className={cx(btn.base, btn.primary)} onClick={() => window.location.reload()}>
          Reload
        </button>
      </div>
    );
  }
}
