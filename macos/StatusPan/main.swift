import AppKit

// NSApplication.delegate does not retain its delegate.
let appDelegate = AppDelegate()

func makeMainMenu(owner: AppDelegate) -> NSMenu {
    let menu = NSMenu()
    menu.addItem(submenuItem(makeAppMenu()))
    menu.addItem(submenuItem(makeEditMenu()))
    menu.addItem(submenuItem(makeViewMenu(owner: owner)))
    menu.addItem(submenuItem(makeWindowMenu()))
    return menu
}

private func submenuItem(_ menu: NSMenu) -> NSMenuItem {
    let item = NSMenuItem(title: menu.title, action: nil, keyEquivalent: "")
    item.submenu = menu
    return item
}

private func item(
    _ title: String,
    action: Selector?,
    key: String = "",
    modifiers: NSEvent.ModifierFlags? = nil,
    target: AnyObject? = nil
) -> NSMenuItem {
    let item = NSMenuItem(title: title, action: action, keyEquivalent: key)
    if let modifiers {
        item.keyEquivalentModifierMask = modifiers
    }
    item.target = target
    return item
}

private func makeAppMenu() -> NSMenu {
    let menu = NSMenu(title: "StatusPan")
    menu.addItem(item("About StatusPan", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:))))
    menu.addItem(.separator())
    menu.addItem(item("Hide StatusPan", action: #selector(NSApplication.hide(_:)), key: "h"))
    menu.addItem(item(
        "Hide Others",
        action: #selector(NSApplication.hideOtherApplications(_:)),
        key: "h",
        modifiers: [.command, .option]
    ))
    menu.addItem(item("Show All", action: #selector(NSApplication.unhideAllApplications(_:))))
    menu.addItem(.separator())
    menu.addItem(item("Quit StatusPan", action: #selector(NSApplication.terminate(_:)), key: "q"))
    return menu
}

private func makeEditMenu() -> NSMenu {
    let menu = NSMenu(title: "Edit")
    menu.addItem(item("Undo", action: Selector(("undo:")), key: "z"))
    menu.addItem(item("Redo", action: Selector(("redo:")), key: "z", modifiers: [.command, .shift]))
    menu.addItem(.separator())
    menu.addItem(item("Cut", action: #selector(NSText.cut(_:)), key: "x"))
    menu.addItem(item("Copy", action: #selector(NSText.copy(_:)), key: "c"))
    menu.addItem(item("Paste", action: #selector(NSText.paste(_:)), key: "v"))
    menu.addItem(item("Select All", action: #selector(NSText.selectAll(_:)), key: "a"))
    return menu
}

private func makeViewMenu(owner: AppDelegate) -> NSMenu {
    let menu = NSMenu(title: "View")
    menu.addItem(item("Reload", action: #selector(AppDelegate.reloadPage(_:)), key: "r", target: owner))
    menu.addItem(.separator())
    menu.addItem(item("Actual Size", action: #selector(AppDelegate.actualSize(_:)), key: "0", target: owner))
    menu.addItem(item("Zoom In", action: #selector(AppDelegate.zoomIn(_:)), key: "=", target: owner))
    menu.addItem(item("Zoom Out", action: #selector(AppDelegate.zoomOut(_:)), key: "-", target: owner))
    return menu
}

private func makeWindowMenu() -> NSMenu {
    let menu = NSMenu(title: "Window")
    menu.addItem(item("Minimize", action: #selector(NSWindow.performMiniaturize(_:)), key: "m"))
    menu.addItem(item("Zoom", action: #selector(NSWindow.performZoom(_:))))
    menu.addItem(.separator())
    menu.addItem(item("Close", action: #selector(NSWindow.performClose(_:)), key: "w"))
    return menu
}

let app = NSApplication.shared
let mainMenu = makeMainMenu(owner: appDelegate)
app.setActivationPolicy(.regular)
app.delegate = appDelegate
app.mainMenu = mainMenu
app.windowsMenu = mainMenu.item(withTitle: "Window")?.submenu
app.run()
