'use client';

import ColumnHeader from '@/components/DualListBox/ColumnHeader';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip';
import { Pencil1Icon, ReloadIcon } from '@radix-ui/react-icons';
import { ColumnDef } from '@tanstack/react-table';
import { ReactElement } from 'react';
import { DataTableColumnHeader } from './data-table-column-header';

export interface TableRow {
  schema: string;
  table: string;
  where?: string;
  isRootTable?: boolean;
}

interface GetColumnsProps {
  onEdit(schema: string, table: string): void;
  hasLocalChange(schema: string, table: string): boolean;
  onReset(schema: string, table: string): void;
}

export function getColumns(props: GetColumnsProps): ColumnDef<TableRow>[] {
  const { onEdit, hasLocalChange, onReset } = props;
  return [
    {
      accessorKey: 'schema',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title="Schema" />
      ),
      cell: ({ getValue }) => getValue<string>(),
    },
    {
      accessorKey: 'table',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title="Table" />
      ),
      cell: ({ getValue }) => {
        return (
          <div className="flex space-x-2">
            <span className="truncate font-medium">{getValue<string>()}</span>
          </div>
        );
      },
    },
    {
      accessorFn: (row) => `${row.schema}.${row.table}`,
      id: 'schemaTable',
      footer: (props) => props.column.id,
      header: ({ column }) => (
        <ColumnHeader
          column={column}
          title={'Table'}
          placeholder={'Search Tables ..'}
        />
      ),
    },
    {
      accessorKey: 'isRootTable',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title="Badges" />
      ),
      cell: ({ getValue }) => {
        return (
          <div className="flex space-x-2">
            {getValue<boolean>() && <Badge>Root</Badge>}
          </div>
        );
      },
    },
    {
      accessorKey: 'where',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title="Where" />
      ),
      cell: ({ getValue }) => {
        return (
          <div className="flex space-x-2">
            <span className="truncate font-medium">{getValue<boolean>()}</span>
          </div>
        );
      },
    },
    {
      accessorKey: 'edit',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title="Actions" />
      ),
      cell: ({ row }) => {
        const schema = row.getValue<string>('schema');
        const table = row.getValue<string>('table');
        return (
          <div className="flex gap-2">
            <EditAction onClick={() => onEdit(schema, table)} />
            <ResetAction
              onClick={() => onReset(schema, table)}
              isDisabled={!hasLocalChange(schema, table)}
            />
          </div>
        );
      },
      enableSorting: false,
      enableColumnFilter: false,
    },
  ];
}

interface EditActionProps {
  onClick(): void;
}

function EditAction(props: EditActionProps): ReactElement {
  const { onClick } = props;
  return (
    <Button
      type="button"
      variant="outline"
      size="icon"
      onClick={() => onClick()}
    >
      <Pencil1Icon />
    </Button>
  );
}

interface ResetActionProps {
  onClick(): void;
  isDisabled: boolean;
}

function ResetAction(props: ResetActionProps): ReactElement {
  const { onClick, isDisabled } = props;
  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            type="button"
            className="scale-x-[-1]"
            variant="outline"
            size="icon"
            onClick={() => onClick()}
            disabled={isDisabled}
          >
            <ReloadIcon />
          </Button>
        </TooltipTrigger>
        <TooltipContent>
          <p>Reset changes made locally to this row</p>
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
