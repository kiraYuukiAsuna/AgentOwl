import { create } from "zustand";
export const useUI = create<{
  commandOpen: boolean;
  mobileOpen: boolean;
  setCommandOpen: (open: boolean) => void;
  setMobileOpen: (open: boolean) => void;
}>((set) => ({
  commandOpen: false,
  mobileOpen: false,
  setCommandOpen: (commandOpen) => set({ commandOpen }),
  setMobileOpen: (mobileOpen) => set({ mobileOpen }),
}));
