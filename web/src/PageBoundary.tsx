import { Component, type ReactNode } from "react";

export class PageBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  render() {
    if (this.state.failed) {
      return <div className="signal-error" role="alert">Unable to load this page. Reload the page to try again.</div>;
    }
    return this.props.children;
  }
}
