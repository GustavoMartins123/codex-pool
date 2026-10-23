import SwiftUI
import UIKit
import ComposeApp

@main
struct PoolApp: App {
    var body: some Scene {
        WindowGroup {
            PoolView().ignoresSafeArea()
        }
    }
}

struct PoolView: UIViewControllerRepresentable {
    func makeUIViewController(context: Context) -> UIViewController {
        MainViewControllerKt.MainViewController()
    }

    func updateUIViewController(_ uiViewController: UIViewController, context: Context) {}
}
