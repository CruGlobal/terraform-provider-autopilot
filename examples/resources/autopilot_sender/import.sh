# Import a sender by its name. AutoPilot never sends its secrets back, so put
# them in configuration: the next plan compares their fingerprints with the
# stored ones, and sends them only if they differ.
terraform import autopilot_sender.tracker tracker
