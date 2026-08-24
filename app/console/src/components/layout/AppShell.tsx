import { Navigate, Outlet } from "react-router-dom";
import { Sidebar } from "./Sidebar";
import { TopBar } from "./TopBar";
import { useSession } from "@/lib/session";
import { PageSpinner } from "@/components/ui/Spinner";

export function AppShell() {
  const { session, loading } = useSession();

  if (loading) return <PageSpinner />;
  if (!session) return <Navigate to="/login" replace />;

  return (
    <div className="flex h-screen w-full overflow-hidden bg-bg text-fg">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar />
        <main className="min-w-0 flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-[1400px] px-6 py-6">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
