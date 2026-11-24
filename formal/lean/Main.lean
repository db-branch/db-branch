import DbBranchAuthz.JsonExport

def main : IO Unit := do
  IO.println DbBranchAuthz.JsonExport.buildMatrixJSON
